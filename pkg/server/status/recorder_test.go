// Copyright 2015 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package status

import (
	"bytes"
	"context"
	"io"
	"os"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/build"
	"github.com/cockroachdb/cockroach/pkg/multitenant"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/server/status/statuspb"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/catconstants"
	"github.com/cockroachdb/cockroach/pkg/ts/tspb"
	"github.com/cockroachdb/cockroach/pkg/ts/tsutil"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/metric"
	"github.com/cockroachdb/cockroach/pkg/util/metric/aggmetric"
	"github.com/cockroachdb/cockroach/pkg/util/system"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/kr/pretty"
	prometheusgo "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

// byTimeAndName is a slice of tspb.TimeSeriesData.
type byTimeAndName []tspb.TimeSeriesData

// implement sort.Interface for byTimeAndName
func (a byTimeAndName) Len() int      { return len(a) }
func (a byTimeAndName) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a byTimeAndName) Less(i, j int) bool {
	if a[i].Name != a[j].Name {
		return a[i].Name < a[j].Name
	}
	if a[i].Datapoints[0].TimestampNanos != a[j].Datapoints[0].TimestampNanos {
		return a[i].Datapoints[0].TimestampNanos < a[j].Datapoints[0].TimestampNanos
	}
	return a[i].Source < a[j].Source
}

var _ sort.Interface = byTimeAndName{}

// byStoreID is a slice of roachpb.StoreID.
type byStoreID []roachpb.StoreID

// implement sort.Interface for byStoreID
func (a byStoreID) Len() int      { return len(a) }
func (a byStoreID) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a byStoreID) Less(i, j int) bool {
	return a[i] < a[j]
}

var _ sort.Interface = byStoreID{}

// byStoreDescID is a slice of storage.StoreStatus
type byStoreDescID []statuspb.StoreStatus

// implement sort.Interface for byStoreDescID.
func (a byStoreDescID) Len() int      { return len(a) }
func (a byStoreDescID) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a byStoreDescID) Less(i, j int) bool {
	return a[i].Desc.StoreID < a[j].Desc.StoreID
}

var _ sort.Interface = byStoreDescID{}

// fakeStore implements only the methods of store needed by MetricsRecorder to
// interact with stores.
type fakeStore struct {
	storeID  roachpb.StoreID
	desc     roachpb.StoreDescriptor
	registry *metric.Registry
}

func (fs fakeStore) StoreID() roachpb.StoreID {
	return fs.storeID
}

func (fs fakeStore) Descriptor(_ context.Context, _ bool) (*roachpb.StoreDescriptor, error) {
	return &fs.desc, nil
}

func (fs fakeStore) Registry() *metric.Registry {
	return fs.registry
}

func TestMetricsRecorderLabels(t *testing.T) {
	defer leaktest.AfterTest(t)()
	nodeDesc := roachpb.NodeDescriptor{
		NodeID: roachpb.NodeID(7),
	}
	reg1 := metric.NewRegistry()
	manual := timeutil.NewManualTime(timeutil.Unix(0, 100))
	st := cluster.MakeTestingClusterSettings()
	recorder := NewMetricsRecorder(
		roachpb.SystemTenantID,
		roachpb.NewTenantNameContainer(catconstants.SystemTenantName),
		nil, /* nodeLiveness */
		nil, /* remoteClocks */
		manual,
		st,
	)
	recorder.AddNode(reg1, nodeDesc, 50, "foo:26257", "foo:26258", "foo:5432")

	nodeDescTenant := roachpb.NodeDescriptor{
		NodeID: roachpb.NodeID(7),
	}
	regTenant := metric.NewRegistry()
	stTenant := cluster.MakeTestingClusterSettings()
	tenantID, err := roachpb.MakeTenantID(123)
	require.NoError(t, err)

	appNameContainer := roachpb.NewTenantNameContainer("application")
	recorderTenant := NewMetricsRecorder(
		tenantID,
		appNameContainer,
		nil, /* nodeLiveness */
		nil, /* remoteClocks */
		manual,
		stTenant,
	)
	recorderTenant.AddNode(regTenant, nodeDescTenant, 50, "foo:26257", "foo:26258", "foo:5432")

	// ========================================
	// Verify that the recorder exports metrics for tenants as text.
	// ========================================

	g := metric.NewGauge(metric.Metadata{Name: "some_metric"})
	reg1.AddMetric(g)
	g.Update(123)

	g2 := metric.NewGauge(metric.Metadata{Name: "some_metric"})
	regTenant.AddMetric(g2)
	g2.Update(456)

	recorder.AddTenantRegistry(tenantID, regTenant)

	buf := bytes.NewBuffer([]byte{})
	err = recorder.PrintAsText(buf)
	require.NoError(t, err)

	require.Contains(t, buf.String(), `some_metric{node_id="7",tenant="system"} 123`)
	require.Contains(t, buf.String(), `some_metric{node_id="7",tenant="application"} 456`)

	bufTenant := bytes.NewBuffer([]byte{})
	err = recorderTenant.PrintAsText(bufTenant)
	require.NoError(t, err)

	require.NotContains(t, bufTenant.String(), `some_metric{node_id="7",tenant="system"} 123`)
	require.Contains(t, bufTenant.String(), `some_metric{node_id="7",tenant="application"} 456`)

	// Update app name in container and ensure
	// output changes accordingly.
	appNameContainer.Set("application2")

	buf = bytes.NewBuffer([]byte{})
	err = recorder.PrintAsText(buf)
	require.NoError(t, err)

	require.Contains(t, buf.String(), `some_metric{node_id="7",tenant="system"} 123`)
	require.Contains(t, buf.String(), `some_metric{node_id="7",tenant="application2"} 456`)

	bufTenant = bytes.NewBuffer([]byte{})
	err = recorderTenant.PrintAsText(bufTenant)
	require.NoError(t, err)

	require.NotContains(t, bufTenant.String(), `some_metric{node_id="7",tenant="system"} 123`)
	require.Contains(t, bufTenant.String(), `some_metric{node_id="7",tenant="application2"} 456`)

	// ========================================
	// Verify that the recorder processes tenant time series registries
	// ========================================

	expectedData := []tspb.TimeSeriesData{
		// System tenant metrics
		{
			Name:   "cr.node.node-id",
			Source: "7",
			Datapoints: []tspb.TimeSeriesDatapoint{
				{
					TimestampNanos: manual.Now().UnixNano(),
					Value:          float64(7),
				},
			},
		},
		{
			Name:   "cr.node.some_metric",
			Source: "7",
			Datapoints: []tspb.TimeSeriesDatapoint{
				{
					TimestampNanos: manual.Now().UnixNano(),
					Value:          float64(123),
				},
			},
		},
		// App tenant metrics
		{
			Name:   "cr.node.node-id",
			Source: "7-123",
			Datapoints: []tspb.TimeSeriesDatapoint{
				{
					TimestampNanos: manual.Now().UnixNano(),
					Value:          float64(nodeDesc.NodeID),
				},
			},
		},
		{
			Name:   "cr.node.some_metric",
			Source: "7-123",
			Datapoints: []tspb.TimeSeriesDatapoint{
				{
					TimestampNanos: manual.Now().UnixNano(),
					Value:          float64(456),
				},
			},
		},
	}

	actualData := recorder.GetTimeSeriesData()

	// compare actual vs expected values
	sort.Sort(byTimeAndName(actualData))
	sort.Sort(byTimeAndName(expectedData))
	if a, e := actualData, expectedData; !reflect.DeepEqual(a, e) {
		t.Errorf("recorder did not yield expected time series collection; diff:\n %v", pretty.Diff(e, a))
	}
}

func TestRegistryRecorder_RecordChild(t *testing.T) {
	defer leaktest.AfterTest(t)()
	store1 := fakeStore{
		storeID: roachpb.StoreID(1),
		desc: roachpb.StoreDescriptor{
			StoreID: roachpb.StoreID(1),
			Capacity: roachpb.StoreCapacity{
				Capacity:  100,
				Available: 50,
				Used:      50,
			},
		},
		registry: metric.NewRegistry(),
	}
	store2 := fakeStore{
		storeID: roachpb.StoreID(2),
		desc: roachpb.StoreDescriptor{
			StoreID: roachpb.StoreID(2),
			Capacity: roachpb.StoreCapacity{
				Capacity:  200,
				Available: 75,
				Used:      125,
			},
		},
		registry: metric.NewRegistry(),
	}
	systemTenantNameContainer := roachpb.NewTenantNameContainer(catconstants.SystemTenantName)
	manual := timeutil.NewManualTime(timeutil.Unix(0, 100))
	st := cluster.MakeTestingClusterSettings()
	recorder := NewMetricsRecorder(roachpb.SystemTenantID, systemTenantNameContainer, nil, nil, manual, st)
	recorder.AddStore(store1)
	recorder.AddStore(store2)

	tenantIDs := []string{"2", "3"}
	type childMetric struct {
		tenantID string
		value    int64
	}
	type testMetric struct {
		name     string
		typ      string
		children []childMetric
	}
	// Each registry will have a copy of the following metrics.
	metrics := []testMetric{
		{
			name: "testAggGauge",
			typ:  "agggauge",
			children: []childMetric{
				{
					tenantID: "2",
					value:    2,
				},
				{
					tenantID: "3",
					value:    5,
				},
			},
		},
		{
			name: "testAggCounter",
			typ:  "aggcounter",
			children: []childMetric{
				{
					tenantID: "2",
					value:    10,
				},
				{
					tenantID: "3",
					value:    17,
				},
			},
		},
	}

	var expected []tspb.TimeSeriesData
	// addExpected generates expected TimeSeriesData for all child metrics.
	addExpected := func(storeID string, metric *testMetric) {
		for _, child := range metric.children {
			expect := tspb.TimeSeriesData{
				Name:   "cr.store." + metric.name,
				Source: tsutil.MakeTenantSource(storeID, child.tenantID),
				Datapoints: []tspb.TimeSeriesDatapoint{
					{
						TimestampNanos: 100,
						Value:          float64(child.value),
					},
				},
			}
			expected = append(expected, expect)
		}
	}

	tIDLabel := multitenant.TenantIDLabel
	for _, store := range []fakeStore{store1, store2} {
		for _, m := range metrics {
			switch m.typ {
			case "aggcounter":
				ac := aggmetric.NewCounter(metric.Metadata{Name: m.name}, tIDLabel)
				store.registry.AddMetric(ac)
				for _, cm := range m.children {
					c := ac.AddChild(cm.tenantID)
					c.Inc(cm.value)
				}
				addExpected(store.storeID.String(), &m)
			case "agggauge":
				ag := aggmetric.NewGauge(metric.Metadata{Name: m.name}, tIDLabel)
				store.registry.AddMetric(ag)
				for _, cm := range m.children {
					c := ag.AddChild(cm.tenantID)
					c.Inc(cm.value)
				}
				addExpected(store.storeID.String(), &m)
			}
		}
	}
	metricFilter := map[string]struct{}{
		"testAggGauge":   {},
		"testAggCounter": {},
	}
	actual := make([]tspb.TimeSeriesData, 0)
	for _, store := range []fakeStore{store1, store2} {
		for _, tID := range tenantIDs {
			tenantStoreRecorder := registryRecorder{
				registry:       store.registry,
				format:         storeTimeSeriesPrefix,
				source:         tsutil.MakeTenantSource(store.storeID.String(), tID),
				timestampNanos: 100,
			}
			tenantStoreRecorder.recordChild(&actual, metricFilter, &prometheusgo.LabelPair{
				Name:  &tIDLabel,
				Value: &tID,
			})
		}
	}
	sort.Sort(byTimeAndName(actual))
	sort.Sort(byTimeAndName(expected))
	if !reflect.DeepEqual(actual, expected) {
		t.Errorf("registryRecorder did not yield expected time series collection for child metrics; diff:\n %v", pretty.Diff(actual, expected))
	}
}

// TestMetricsRecorder verifies that the metrics recorder properly formats the
// statistics from various registries, both for Time Series and for Status
// Summaries.
func TestMetricsRecorder(t *testing.T) {
	defer leaktest.AfterTest(t)()

	// ========================================
	// Construct a series of fake descriptors for use in test.
	// ========================================
	nodeDesc := roachpb.NodeDescriptor{
		NodeID: roachpb.NodeID(1),
	}
	storeDesc1 := roachpb.StoreDescriptor{
		StoreID: roachpb.StoreID(1),
		Capacity: roachpb.StoreCapacity{
			Capacity:  100,
			Available: 50,
			Used:      50,
		},
	}
	storeDesc2 := roachpb.StoreDescriptor{
		StoreID: roachpb.StoreID(2),
		Capacity: roachpb.StoreCapacity{
			Capacity:  200,
			Available: 75,
			Used:      125,
		},
	}

	// ========================================
	// Create registries and add them to the recorder (two node-level, two
	// store-level).
	// ========================================
	reg1 := metric.NewRegistry()
	store1 := fakeStore{
		storeID:  roachpb.StoreID(1),
		desc:     storeDesc1,
		registry: metric.NewRegistry(),
	}
	store2 := fakeStore{
		storeID:  roachpb.StoreID(2),
		desc:     storeDesc2,
		registry: metric.NewRegistry(),
	}
	manual := timeutil.NewManualTime(timeutil.Unix(0, 100))
	st := cluster.MakeTestingClusterSettings()
	recorder := NewMetricsRecorder(roachpb.SystemTenantID, roachpb.NewTenantNameContainer(""), nil, nil, manual, st)
	recorder.AddStore(store1)
	recorder.AddStore(store2)
	recorder.AddNode(reg1, nodeDesc, 50, "foo:26257", "foo:26258", "foo:5432")

	// Ensure the metric system's view of time does not advance during this test
	// as the test expects time to not advance too far which would age the actual
	// data (e.g. in histogram's) unexpectedly.
	defer metric.TestingSetNow(func() time.Time {
		return manual.Now()
	})()

	// ========================================
	// Generate Metrics Data & Expected Results
	// ========================================

	// Flatten the four registries into an array for ease of use.
	regList := []struct {
		reg    *metric.Registry
		prefix string
		source int64
		isNode bool
	}{
		{
			reg:    reg1,
			prefix: "one.",
			source: 1,
			isNode: true,
		},
		{
			reg:    reg1,
			prefix: "two.",
			source: 1,
			isNode: true,
		},
		{
			reg:    store1.registry,
			prefix: "",
			source: int64(store1.storeID),
			isNode: false,
		},
		{
			reg:    store2.registry,
			prefix: "",
			source: int64(store2.storeID),
			isNode: false,
		},
	}

	// Every registry will have a copy of the following metrics.
	metricNames := []struct {
		name string
		typ  string
		val  int64
	}{
		{"testGauge", "gauge", 20},
		{"testGaugeFloat64", "floatgauge", 20},
		{"testCounter", "counter", 5},
		{"testHistogram", "histogram", 9},
		{"testAggGauge", "agggauge", 4},
		{"testAggCounter", "aggcounter", 7},

		// Stats needed for store summaries.
		{"replicas.leaders", "gauge", 1},
		{"replicas.leaseholders", "gauge", 1},
		{"ranges", "gauge", 1},
		{"ranges.unavailable", "gauge", 1},
		{"ranges.underreplicated", "gauge", 1},
	}

	// Add the metrics to each registry and set their values. At the same time,
	// generate expected time series results and status summary metric values.
	var expected []tspb.TimeSeriesData
	expectedNodeSummaryMetrics := make(map[string]float64)
	expectedStoreSummaryMetrics := make(map[string]float64)

	// addExpected generates expected data for a single metric data point.
	addExpected := func(prefix, name string, source, time, val int64, isNode bool) {
		// Generate time series data.
		tsPrefix := "cr.node."
		if !isNode {
			tsPrefix = "cr.store."
		}
		expect := tspb.TimeSeriesData{
			Name:   tsPrefix + prefix + name,
			Source: strconv.FormatInt(source, 10),
			Datapoints: []tspb.TimeSeriesDatapoint{
				{
					TimestampNanos: time,
					Value:          float64(val),
				},
			},
		}
		expected = append(expected, expect)

		// Generate status summary data.
		if isNode {
			expectedNodeSummaryMetrics[prefix+name] = float64(val)
		} else {
			// This can overwrite the previous value, but this is expected as
			// all stores in our tests have identical values; when comparing
			// status summaries, the same map is used as expected data for all
			// stores.
			expectedStoreSummaryMetrics[prefix+name] = float64(val)
		}
	}

	// Add metric for node ID.
	g := metric.NewGauge(metric.Metadata{Name: "node-id"})
	g.Update(int64(nodeDesc.NodeID))
	addExpected("", "node-id", 1, 100, g.Value(), true)

	for _, reg := range regList {
		for _, data := range metricNames {
			switch data.typ {
			case "gauge":
				g := metric.NewGauge(metric.Metadata{Name: reg.prefix + data.name})
				reg.reg.AddMetric(g)
				g.Update(data.val)
				addExpected(reg.prefix, data.name, reg.source, 100, data.val, reg.isNode)
			case "floatgauge":
				g := metric.NewGaugeFloat64(metric.Metadata{Name: reg.prefix + data.name})
				reg.reg.AddMetric(g)
				g.Update(float64(data.val))
				addExpected(reg.prefix, data.name, reg.source, 100, data.val, reg.isNode)
			case "counter":
				c := metric.NewCounter(metric.Metadata{Name: reg.prefix + data.name})
				reg.reg.AddMetric(c)
				c.Inc((data.val))
				addExpected(reg.prefix, data.name, reg.source, 100, data.val, reg.isNode)
			case "aggcounter":
				ac := aggmetric.NewCounter(metric.Metadata{Name: reg.prefix + data.name}, "foo")
				reg.reg.AddMetric(ac)
				c := ac.AddChild("bar")
				c.Inc((data.val))
				addExpected(reg.prefix, data.name, reg.source, 100, data.val, reg.isNode)
			case "agggauge":
				ac := aggmetric.NewGauge(metric.Metadata{Name: reg.prefix + data.name}, "foo")
				reg.reg.AddMetric(ac)
				c := ac.AddChild("bar")
				c.Inc((data.val))
				addExpected(reg.prefix, data.name, reg.source, 100, data.val, reg.isNode)
			case "histogram":
				h := metric.NewHistogram(metric.HistogramOptions{
					Metadata: metric.Metadata{Name: reg.prefix + data.name},
					Duration: time.Second,
					Buckets:  []float64{1.0, 10.0, 100.0, 1000.0},
					Mode:     metric.HistogramModePrometheus,
				})
				reg.reg.AddMetric(h)
				h.RecordValue(data.val)
				for _, q := range recordHistogramQuantiles {
					addExpected(reg.prefix, data.name+q.suffix, reg.source, 100, 10, reg.isNode)
				}
				addExpected(reg.prefix, data.name+"-count", reg.source, 100, 1, reg.isNode)
				addExpected(reg.prefix, data.name+"-avg", reg.source, 100, 9, reg.isNode)
			default:
				t.Fatalf("unexpected: %+v", data)
			}
		}
	}

	// ========================================
	// Verify time series data
	// ========================================
	actual := recorder.GetTimeSeriesData()

	// Actual comparison is simple: sort the resulting arrays by time and name,
	// and use reflect.DeepEqual.
	sort.Sort(byTimeAndName(actual))
	sort.Sort(byTimeAndName(expected))
	if a, e := actual, expected; !reflect.DeepEqual(a, e) {
		t.Errorf("recorder did not yield expected time series collection; diff:\n %v", pretty.Diff(e, a))
	}

	totalMemory, err := GetTotalMemory(context.Background())
	if err != nil {
		t.Error("couldn't get total memory", err)
	}

	// ========================================
	// Verify node summary generation
	// ========================================
	expectedNodeSummary := &statuspb.NodeStatus{
		Desc:      nodeDesc,
		BuildInfo: build.GetInfo(),
		StartedAt: 50,
		UpdatedAt: 100,
		Metrics:   expectedNodeSummaryMetrics,
		StoreStatuses: []statuspb.StoreStatus{
			{
				Desc:    storeDesc1,
				Metrics: expectedStoreSummaryMetrics,
			},
			{
				Desc:    storeDesc2,
				Metrics: expectedStoreSummaryMetrics,
			},
		},
		TotalSystemMemory: totalMemory,
		NumCpus:           int32(system.NumCPU()),
	}

	// Make sure there is at least one environment variable that will be
	// reported.
	if err := os.Setenv("GOGC", "100"); err != nil {
		t.Fatal(err)
	}

	nodeSummary := recorder.GenerateNodeStatus(context.Background())
	if nodeSummary == nil {
		t.Fatalf("recorder did not return nodeSummary")
	}
	if len(nodeSummary.Args) == 0 {
		t.Fatalf("expected args to be present")
	}
	if len(nodeSummary.Env) == 0 {
		t.Fatalf("expected env to be present")
	}
	nodeSummary.Args = nil
	nodeSummary.Env = nil
	nodeSummary.Activity = nil
	nodeSummary.Latencies = nil

	sort.Sort(byStoreDescID(nodeSummary.StoreStatuses))
	if a, e := nodeSummary, expectedNodeSummary; !reflect.DeepEqual(a, e) {
		t.Errorf("recorder did not produce expected NodeSummary; diff:\n %s", pretty.Diff(e, a))
	}

	// Make sure that all methods other than GenerateNodeStatus can operate in
	// parallel with each other (i.e. even if recorder.mu is RLocked).
	recorder.mu.RLock()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			if _, err := recorder.MarshalJSON(); err != nil {
				t.Error(err)
			}
			_ = recorder.PrintAsText(io.Discard)
			_ = recorder.GetTimeSeriesData()
			wg.Done()
		}()
	}
	wg.Wait()
	recorder.mu.RUnlock()
}
