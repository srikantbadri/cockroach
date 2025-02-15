// Copyright 2023 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package server

import (
	"context"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/clusterversion"
	"github.com/cockroachdb/cockroach/pkg/multitenant/mtinfopb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/isql"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/catconstants"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/log/eventpb"
	"github.com/cockroachdb/cockroach/pkg/util/log/logpb"
	"github.com/cockroachdb/cockroach/pkg/util/retry"
	"github.com/cockroachdb/cockroach/pkg/util/startup"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/logtags"
	"github.com/cockroachdb/redact"
)

// serverState coordinates the lifecycle of a tenant server. It ensures
// sane concurrent behavior between:
// - requests to start a server manually, e.g. via TestServer;
// - async changes to the tenant service mode;
// - quiescence of the outer stopper;
// - RPC drain requests on the tenant server;
// - server startup errors if any.
//
// Generally, the lifecycle is as follows:
//  1. a request to start a server will cause a serverEntry to be added
//     to the server controller, in the state "not yet started".
//  2. the "managed-tenant-server" async task starts, via
//     StartControlledServer()
//  3. the async task attempts to start the server (with retries and
//     backoff delay as needed), or cancels the startup if
//     a request to stop is received asynchronously.
//  4. after the server is started, the async task waits for a shutdown
//     request.
//  5. once a shutdown request is received the async task
//     stops the server.
//
// The async task is also responsible for reporting the server
// start/stop events in the event log.
type serverState struct {
	// startedOrStopped is closed when the server has either started or
	// stopped. This can be used to wait for a server start.
	startedOrStopped <-chan struct{}

	// startErr, once startedOrStopped is closed, reports the error
	// during server creation if any.
	startErr error

	// started is marked true when the server has started. This can
	// be used to observe the start state without waiting.
	started syncutil.AtomicBool

	// requestStop can be called to request a server to stop.
	// It can be called multiple times.
	requestStop func()

	// stopped is closed when the server has stopped.
	stopped <-chan struct{}
}

// start monitors changes to the service mode and updates
// the running servers accordingly.
func (c *serverController) start(ctx context.Context, ie isql.Executor) error {
	// We perform one round of updates synchronously, to ensure that
	// any tenants already in service mode SHARED get a chance to boot
	// up before we signal readiness.
	if err := c.startInitialSecondaryTenantServers(ctx, ie); err != nil {
		return err
	}

	// Run the detection of which servers should be started or stopped.
	return c.stopper.RunAsyncTask(ctx, "mark-tenant-services", func(ctx context.Context) {
		const watchInterval = time.Second
		ctx, cancel := c.stopper.WithCancelOnQuiesce(ctx)
		defer cancel()
		for {
			select {
			case <-time.After(watchInterval):
			case <-c.stopper.ShouldQuiesce():
				// Expedited server shutdown of outer server.
				return
			}
			if c.draining.Get() {
				// The outer server has started a graceful drain: stop
				// picking up new servers.
				return
			}
			if err := c.scanTenantsForRunnableServices(ctx, ie); err != nil {
				log.Warningf(ctx, "cannot update running tenant services: %v", err)
			}
		}
	})
}

// startInitialSecondaryTenantServers starts the servers for secondary tenants
// that should be started during server initialization.
func (c *serverController) startInitialSecondaryTenantServers(
	ctx context.Context, ie isql.Executor,
) error {
	// The list of tenants that should have a running server.
	reqTenants, err := startup.RunIdempotentWithRetryEx(ctx,
		c.stopper.ShouldQuiesce(),
		"get expected running tenants",
		func(ctx context.Context) ([]roachpb.TenantName, error) {
			return c.getExpectedRunningTenants(ctx, ie)
		})
	if err != nil {
		return err
	}
	for _, name := range reqTenants {
		if name == catconstants.SystemTenantName {
			// We already pre-initialize the entry for the system tenant.
			continue
		}
		if _, err := c.startAndWaitForRunningServer(ctx, name); err != nil {
			return err
		}
	}
	return nil
}

// scanTenantsForRunnableServices checks which tenants need to be
// started/stopped and queues the necessary server lifecycle changes.
func (c *serverController) scanTenantsForRunnableServices(
	ctx context.Context, ie isql.Executor,
) error {
	// The list of tenants that should have a running server.
	reqTenants, err := c.getExpectedRunningTenants(ctx, ie)
	if err != nil {
		return err
	}

	// Create a lookup map for the first loop below.
	nameLookup := make(map[roachpb.TenantName]struct{}, len(reqTenants))
	for _, name := range reqTenants {
		nameLookup[name] = struct{}{}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// First check if there are any servers that shouldn't be running
	// right now.
	for name, srv := range c.mu.servers {
		if _, ok := nameLookup[name]; !ok {
			log.Infof(ctx, "tenant %q has changed service mode, should now stop", name)
			// Mark the server for async shutdown.
			srv.state.requestStop()
		}
	}

	// Now add all the missing servers.
	for _, name := range reqTenants {
		if _, ok := c.mu.servers[name]; !ok {
			log.Infof(ctx, "tenant %q has changed service mode, should now start", name)
			// Mark the server for async creation.
			if _, err := c.createServerEntryLocked(ctx, name); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *serverController) createServerEntryLocked(
	ctx context.Context, tenantName roachpb.TenantName,
) (*serverEntry, error) {
	if c.draining.Get() {
		return nil, errors.New("server is draining")
	}
	entry, err := c.startControlledServer(ctx, tenantName)
	if err != nil {
		return nil, err
	}
	c.mu.servers[tenantName] = entry
	return entry, nil
}

// startControlledServer starts the orchestration task that starts,
// then shuts down, the server for the given tenant.
func (c *serverController) startControlledServer(
	ctx context.Context, tenantName roachpb.TenantName,
) (*serverEntry, error) {
	var stoppedChClosed syncutil.AtomicBool
	stopRequestCh := make(chan struct{})
	stoppedCh := make(chan struct{})
	startedOrStoppedCh := make(chan struct{})
	entry := &serverEntry{
		nameContainer: roachpb.NewTenantNameContainer(tenantName),
		state: serverState{
			startedOrStopped: startedOrStoppedCh,
			requestStop: func() {
				if !stoppedChClosed.Swap(true) {
					close(stopRequestCh)
				}
			},
			stopped: stoppedCh,
		},
	}

	topCtx := ctx

	// Use a different context for the tasks below, because the tenant
	// stopper will have its own tracer which is incompatible with the
	// tracer attached to the incoming context.
	tenantCtx := logtags.WithTags(context.Background(), logtags.FromContext(ctx))
	tenantCtx = logtags.AddTag(tenantCtx, "tenant-orchestration", nil)
	tenantCtx = logtags.AddTag(tenantCtx, "tenant", tenantName)

	// ctlStopper is a stopper uniquely responsible for the control
	// loop. It is separate from the tenantStopper defined below so
	// that we can retry the server instantiation if it fails.
	ctlStopper := stop.NewStopper()

	// useGracefulDrainDuringTenantShutdown defined whether a graceful
	// drain is requested on the tenant server by orchestration.
	useGracefulDrainDuringTenantShutdown := make(chan bool, 1)

	// Ensure that if the surrounding server requests shutdown, we
	// propagate it to the new server.
	if err := c.stopper.RunAsyncTask(ctx, "propagate-close", func(ctx context.Context) {
		select {
		case <-stoppedCh:
			// Server control loop is terminating prematurely before a
			// request was made to terminate it.
			log.Infof(ctx, "tenant %q terminating", tenantName)

		case <-c.stopper.ShouldQuiesce():
			// Surrounding server is stopping; propagate the stop to the
			// control goroutine below.
			// Note: we can't do a graceful drain in that case because
			// the RPC service in the surrounding server may already
			// be unavailable.
			log.Infof(ctx, "server terminating; telling tenant %q to terminate", tenantName)
			useGracefulDrainDuringTenantShutdown <- false
			ctlStopper.Stop(tenantCtx)

		case <-stopRequestCh:
			// Someone requested a graceful shutdown.
			log.Infof(ctx, "received request for tenant %q to terminate", tenantName)
			useGracefulDrainDuringTenantShutdown <- true
			ctlStopper.Stop(tenantCtx)

		case <-topCtx.Done():
			// Someone requested a shutdown - probably a test.
			// Note: we can't do a graceful drain in that case because
			// the RPC service in the surrounding server may already
			// be unavailable.
			log.Infof(ctx, "startup context cancelled; telling tenant %q to terminate", tenantName)
			useGracefulDrainDuringTenantShutdown <- false
			ctlStopper.Stop(tenantCtx)
		}
	}); err != nil {
		// The goroutine above is responsible for stopping the ctlStopper.
		// If it fails to stop, we stop it here to avoid leaking a
		// stopper.
		ctlStopper.Stop(ctx)
		return nil, err
	}

	if err := c.stopper.RunAsyncTask(ctx, "managed-tenant-server", func(_ context.Context) {
		startedOrStoppedChAlreadyClosed := false
		defer func() {
			// We may be returning early due to an error in the server initialization
			// not otherwise caused by a server shutdown. In that case, we don't have
			// a guarantee that the tenantStopper.Stop() call will ever be called
			// and we could get a goroutine leak for the above task.
			// To prevent this, we call requestStop() which tells the goroutine above
			// to call tenantStopper.Stop() and terminate.
			entry.state.requestStop()
			entry.state.started.Set(false)
			close(stoppedCh)
			if !startedOrStoppedChAlreadyClosed {
				entry.state.startErr = errors.New("server stop before successful start")
				close(startedOrStoppedCh)
			}

			// Remove the entry from the server map.
			c.mu.Lock()
			defer c.mu.Unlock()
			delete(c.mu.servers, tenantName)
		}()

		// We use our detached tenantCtx, the incoming ctx given by
		// RunAsyncTask, because this stopper will be assigned its own
		// different tracer.
		ctx := tenantCtx
		// We want a context that gets cancelled when the server is
		// shutting down, for the possible few cases in
		// newServerInternal/preStart/acceptClients which are not looking at the
		// tenantStopper.ShouldQuiesce() channel but are sensitive to context
		// cancellation.
		var cancel func()
		ctx, cancel = ctlStopper.WithCancelOnQuiesce(ctx)
		defer cancel()

		// Stop retrying startup/initialization if we are being shut
		// down early.
		retryOpts := retry.Options{
			Closer: ctlStopper.ShouldQuiesce(),
		}

		// tenantStopper is the stopper specific to one tenant server
		// instance. We define a new tenantStopper on every attempt to
		// instantiate the tenant server below. It is then linked to
		// ctlStopper below once the instantiation and start have
		// succeeded.
		var tenantStopper *stop.Stopper

		var tenantServer onDemandServer
		for retry := retry.StartWithCtx(ctx, retryOpts); retry.Next(); {
			tenantStopper = stop.NewStopper()

			// Link the controller stopper to this tenant stopper.
			if err := ctlStopper.RunAsyncTask(ctx, "propagate-close-tenant", func(ctx context.Context) {
				select {
				case <-tenantStopper.ShouldQuiesce():
					// Tenant server shutting down on its own.
					return
				case <-ctlStopper.ShouldQuiesce():
					select {
					case gracefulDrainRequested := <-useGracefulDrainDuringTenantShutdown:
						if gracefulDrainRequested {
							// Ensure that the graceful drain for the tenant server aborts
							// early if the Stopper for the surrounding server is
							// prematurely shutting down. This is because once the surrounding node
							// starts quiescing tasks, it won't be able to process KV requests
							// by the tenant server any more.
							//
							// Beware: we use tenantCtx here, not ctx, because the
							// latter has been linked to ctlStopper.Quiesce already
							// -- and in this select branch that context has been
							// canceled already.
							drainCtx, cancel := c.stopper.WithCancelOnQuiesce(tenantCtx)
							defer cancel()
							log.Infof(drainCtx, "starting graceful drain")
							// Call the drain service on that tenant's server. This may take a
							// while as it needs to wait for clients to disconnect and SQL
							// activity to clear up, possibly waiting for various configurable
							// timeouts.
							CallDrainServerSide(drainCtx, tenantServer.gracefulDrain)
						}
					default:
					}
					tenantStopper.Stop(ctx)
				case <-c.stopper.ShouldQuiesce():
					// Expedited shutdown of the surrounding KV node.
					tenantStopper.Stop(ctx)
				}
			}); err != nil {
				tenantStopper.Stop(ctx)
				return
			}

			// Try to create the server.
			s, err := func() (onDemandServer, error) {
				s, err := c.newServerInternal(ctx, entry.nameContainer, tenantStopper)
				if err != nil {
					return nil, errors.Wrap(err, "while creating server")
				}

				// Note: we make preStart() below derive from ctx, which is
				// cancelled on shutdown of the outer server. This is necessary
				// to ensure preStart() properly stops prematurely in that case.
				startCtx := s.annotateCtx(ctx)
				startCtx = logtags.AddTag(startCtx, "start-server", nil)
				log.Infof(startCtx, "starting tenant server")
				if err := s.preStart(startCtx); err != nil {
					return nil, errors.Wrap(err, "while starting server")
				}
				return s, errors.Wrap(s.acceptClients(startCtx), "while accepting clients")
			}()
			if err != nil {
				// Creation failed. We stop the tenant stopper here, which also
				// takes care of terminating the async task we've just started above.
				tenantStopper.Stop(ctx)
				c.logStartEvent(ctx, roachpb.TenantID{}, 0,
					entry.nameContainer.Get(), false /* success */, err)
				log.Warningf(ctx,
					"unable to start server for tenant %q (attempt %d, will retry): %v",
					tenantName, retry.CurrentAttempt(), err)
				entry.state.startErr = err
				continue
			}
			tenantServer = s
			break
		}
		if tenantServer == nil {
			// Retry loop exited before the server could start. This
			// indicates that there was an async request to abandon the
			// server startup. This is OK; just terminate early. The defer
			// will take care of cleaning up.
			return
		}

		// Log the start event and ensure the stop event is logged eventually.
		tid, iid := tenantServer.getTenantID(), tenantServer.getInstanceID()
		c.logStartEvent(ctx, tid, iid, tenantName, true /* success */, nil)
		tenantStopper.AddCloser(stop.CloserFn(func() {
			c.logStopEvent(ctx, tid, iid, tenantName)
		}))

		// Indicate the server has started.
		entry.server = tenantServer
		startedOrStoppedChAlreadyClosed = true
		entry.state.started.Set(true)
		close(startedOrStoppedCh)

		// Wait for a request to shut down.
		select {
		case <-tenantStopper.ShouldQuiesce():
			log.Infof(ctx, "tenant %q finishing their own control loop", tenantName)

		case shutdownRequest := <-tenantServer.shutdownRequested():
			log.Infof(ctx, "tenant %q requesting their own shutdown: %v",
				tenantName, shutdownRequest.ShutdownCause())
			// Make the async stop goroutine above pick up the task of shutting down.
			entry.state.requestStop()
		}
	}); err != nil {
		// Clean up the task we just started before.
		entry.state.requestStop()
		return nil, err
	}

	return entry, nil
}

// getExpectedRunningTenants retrieves the tenant IDs that should
// be running right now.
// TODO(knz): Use a watcher here.
// Probably as followup to https://github.com/cockroachdb/cockroach/pull/95657.
func (c *serverController) getExpectedRunningTenants(
	ctx context.Context, ie isql.Executor,
) (tenantNames []roachpb.TenantName, resErr error) {
	if !c.st.Version.IsActive(ctx, clusterversion.V23_1TenantNamesStateAndServiceMode) {
		// Cluster not yet upgraded - we know there is no secondary tenant
		// with a name yet.
		return []roachpb.TenantName{catconstants.SystemTenantName}, nil
	}

	rowIter, err := ie.QueryIterator(ctx, "list-tenants", nil, /* txn */
		`SELECT name FROM system.tenants
WHERE service_mode = $1
  AND data_state = $2
  AND name IS NOT NULL
ORDER BY name`, mtinfopb.ServiceModeShared, mtinfopb.DataStateReady)
	if err != nil {
		return nil, err
	}
	defer func() { resErr = errors.CombineErrors(resErr, rowIter.Close()) }()

	var hasNext bool
	for hasNext, err = rowIter.Next(ctx); hasNext && err == nil; hasNext, err = rowIter.Next(ctx) {
		row := rowIter.Cur()
		tenantName := tree.MustBeDString(row[0])
		tenantNames = append(tenantNames, roachpb.TenantName(tenantName))
	}
	return tenantNames, err
}

// startAndWaitForRunningServer either waits for an existing server to
// have started already for the given tenant, or starts and wait for a
// new server.
func (c *serverController) startAndWaitForRunningServer(
	ctx context.Context, tenantName roachpb.TenantName,
) (onDemandServer, error) {
	entry, err := func() (*serverEntry, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if entry, ok := c.mu.servers[tenantName]; ok {
			return entry, nil
		}
		return c.createServerEntryLocked(ctx, tenantName)
	}()
	if err != nil {
		return nil, err
	}

	select {
	case <-entry.state.startedOrStopped:
		return entry.server, entry.state.startErr
	case <-c.stopper.ShouldQuiesce():
		return nil, errors.New("server stopping")
	case <-ctx.Done():
		return nil, errors.WithStack(ctx.Err())
	}
}

func (c *serverController) newServerInternal(
	ctx context.Context, nameContainer *roachpb.TenantNameContainer, tenantStopper *stop.Stopper,
) (onDemandServer, error) {
	tenantName := nameContainer.Get()
	testArgs := c.testArgs[tenantName]

	// Server does not exist yet: instantiate and start it.
	idx := func() int {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.mu.nextServerIdx++
		return c.mu.nextServerIdx
	}()
	return c.tenantServerCreator.newTenantServer(ctx, nameContainer, tenantStopper, idx, testArgs)
}

// Close implements the stop.Closer interface.
func (c *serverController) Close() {
	entries := c.requestStopAll()

	// Wait for shutdown for all servers.
	for _, e := range entries {
		<-e.state.stopped
	}
}

func (c *serverController) drain(ctx context.Context) (stillRunning int) {
	entries := c.requestStopAll()
	// How many entries are _not_ stopped yet?
	notStopped := 0
	for _, e := range entries {
		select {
		case <-e.state.stopped:
		default:
			log.Infof(ctx, "server for tenant %q still running", e.nameContainer)
			notStopped++
		}
	}
	return notStopped
}

func (c *serverController) requestStopAll() []*serverEntry {
	entries := func() (res []*serverEntry) {
		c.mu.Lock()
		defer c.mu.Unlock()
		res = make([]*serverEntry, 0, len(c.mu.servers))
		for _, e := range c.mu.servers {
			res = append(res, e)
		}
		return res
	}()

	// Request shutdown for all servers.
	for _, e := range entries {
		e.state.requestStop()
	}
	return entries
}

type nodeEventLogger interface {
	logStructuredEvent(ctx context.Context, event logpb.EventPayload)
}

func (c *serverController) logStartEvent(
	ctx context.Context,
	tid roachpb.TenantID,
	instanceID base.SQLInstanceID,
	tenantName roachpb.TenantName,
	success bool,
	opError error,
) {
	ev := &eventpb.TenantSharedServiceStart{OK: success}
	if opError != nil {
		ev.ErrorText = redact.Sprint(opError)
	}
	sharedDetails := &ev.CommonSharedServiceEventDetails
	sharedDetails.NodeID = int32(c.nodeID.Get())
	if tid.IsSet() {
		sharedDetails.TenantID = tid.ToUint64()
	}
	sharedDetails.InstanceID = int32(instanceID)
	sharedDetails.TenantName = string(tenantName)

	c.logger.logStructuredEvent(ctx, ev)
}

func (c *serverController) logStopEvent(
	ctx context.Context,
	tid roachpb.TenantID,
	instanceID base.SQLInstanceID,
	tenantName roachpb.TenantName,
) {
	ev := &eventpb.TenantSharedServiceStop{}
	sharedDetails := &ev.CommonSharedServiceEventDetails
	sharedDetails.NodeID = int32(c.nodeID.Get())
	if tid.IsSet() {
		sharedDetails.TenantID = tid.ToUint64()
	}
	sharedDetails.InstanceID = int32(instanceID)
	sharedDetails.TenantName = string(tenantName)

	c.logger.logStructuredEvent(ctx, ev)
}
