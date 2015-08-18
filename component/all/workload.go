// Copyright 2015 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package all

import (
	"reflect"
	"time"

	"github.com/juju/cmd"
	"github.com/juju/errors"
	"github.com/juju/loggo"
	"github.com/juju/names"
	"gopkg.in/juju/charm.v5"

	"github.com/juju/juju/api/base"
	apiserverclient "github.com/juju/juju/apiserver/client"
	"github.com/juju/juju/apiserver/common"
	cmdstatus "github.com/juju/juju/cmd/juju/status"
	"github.com/juju/juju/cmd/jujud/agent/unit"
	cmdutil "github.com/juju/juju/cmd/jujud/util"
	"github.com/juju/juju/state"
	"github.com/juju/juju/worker"
	"github.com/juju/juju/worker/dependency"
	"github.com/juju/juju/worker/uniter/runner"
	"github.com/juju/juju/worker/uniter/runner/jujuc"
	"github.com/juju/juju/worker/util"
	"github.com/juju/juju/workload"
	"github.com/juju/juju/workload/api/client"
	"github.com/juju/juju/workload/api/server"
	"github.com/juju/juju/workload/context"
	workloadstate "github.com/juju/juju/workload/state"
	"github.com/juju/juju/workload/status"
	"github.com/juju/juju/workload/workers"
)

var workloadsLogger = loggo.GetLogger("component.all.workload")

type workloads struct{}

func (c workloads) registerForServer() error {
	c.registerState()
	handlers := c.registerUnitWorkers()
	c.registerHookContext(handlers)
	c.registerUnitStatus()
	return nil
}

func (workloads) registerForClient() error {
	cmdstatus.RegisterUnitStatusFormatter(workload.ComponentName, status.Format)
	return nil
}

func (c workloads) registerHookContext(handlers map[string]*workers.EventHandlers) {
	if !markRegistered(workload.ComponentName, "hook-context") {
		return
	}

	runner.RegisterComponentFunc(workload.ComponentName,
		func(unit string, caller base.APICaller) (jujuc.ContextComponent, error) {
			var addEvents func(...workload.Event)
			if unitEventHandler, ok := handlers[unit]; ok {
				addEvents = unitEventHandler.AddEvents
			}
			hctxClient := c.newHookContextAPIClient(caller)
			// TODO(ericsnow) Pass the unit's tag through to the component?
			component, err := context.NewContextAPI(hctxClient, addEvents)
			if err != nil {
				return nil, errors.Trace(err)
			}
			return component, nil
		},
	)

	c.registerHookContextCommands()
	c.registerHookContextFacade()
}

func (c workloads) newHookContextAPIClient(caller base.APICaller) context.APIClient {
	facadeCaller := base.NewFacadeCallerForVersion(caller, workload.ComponentName, 0)
	return client.NewHookContextClient(facadeCaller)
}

func (workloads) registerHookContextFacade() {

	newHookContextApi := func(st *state.State, unit *state.Unit) (interface{}, error) {
		if st == nil {
			return nil, errors.NewNotValid(nil, "st is nil")
		}

		up, err := st.UnitWorkloads(unit)
		if err != nil {
			return nil, errors.Trace(err)
		}
		return server.NewHookContextAPI(up), nil
	}

	common.RegisterHookContextFacade(
		workload.ComponentName,
		0,
		newHookContextApi,
		reflect.TypeOf(&server.HookContextAPI{}),
	)
}

type workloadsHookContext struct {
	jujuc.Context
}

// Component implements context.HookContext.
func (c workloadsHookContext) Component(name string) (context.Component, error) {
	found, err := c.Context.Component(name)
	if err != nil {
		return nil, errors.Trace(err)
	}
	compCtx, ok := found.(context.Component)
	if !ok && found != nil {
		return nil, errors.Errorf("wrong component context type registered: %T", found)
	}
	return compCtx, nil
}

func (workloads) registerHookContextCommands() {
	if !markRegistered(workload.ComponentName, "hook-context-commands") {
		return
	}

	name := context.TrackCommandInfo.Name
	jujuc.RegisterCommand(name, func(ctx jujuc.Context) cmd.Command {
		compCtx := workloadsHookContext{ctx}
		cmd, err := context.NewWorkloadTrackCommand(compCtx)
		if err != nil {
			// TODO(ericsnow) Return an error instead.
			panic(err)
		}
		return cmd
	})

	name = context.LaunchCommandInfo.Name
	jujuc.RegisterCommand(name, func(ctx jujuc.Context) cmd.Command {
		compCtx := workloadsHookContext{ctx}
		cmd, err := context.NewWorkloadLaunchCommand(compCtx)
		if err != nil {
			panic(err)
		}
		return cmd
	})

	name = context.InfoCommandInfo.Name
	jujuc.RegisterCommand(name, func(ctx jujuc.Context) cmd.Command {
		compCtx := workloadsHookContext{ctx}
		cmd, err := context.NewWorkloadInfoCommand(compCtx)
		if err != nil {
			panic(err)
		}
		return cmd
	})
}

// TODO(ericsnow) Use a watcher instead of passing around the event handlers?

func (c workloads) registerUnitWorkers() map[string]*workers.EventHandlers {
	if !markRegistered(workload.ComponentName, "workers") {
		return nil
	}

	// TODO(ericsnow) There should only be one...
	unitEventHandlers := make(map[string]*workers.EventHandlers)

	handlerFuncs := []func([]workload.Event, context.APIClient, workers.Runner) error{
		workers.StatusEventHandler,
	}

	newManifold := func(config unit.ManifoldsConfig) (dependency.Manifold, error) {
		// At this point no workload workers are running for the unit.

		unitName := config.Agent.CurrentConfig().Tag().String()
		if unitHandler, ok := unitEventHandlers[unitName]; ok {
			// The worker must have restarted.
			// TODO(ericsnow) Could cause panics?
			unitHandler.Close()
		}

		unitHandler := workers.NewEventHandlers()
		for _, handlerFunc := range handlerFuncs {
			unitHandler.RegisterHandler(handlerFunc)
		}
		unitEventHandlers[unitName] = unitHandler

		apiConfig := util.ApiManifoldConfig{
			APICallerName: unit.APICallerName,
		}
		manifold := util.ApiManifold(apiConfig, func(caller base.APICaller) (worker.Worker, error) {
			apiClient := c.newHookContextAPIClient(caller)

			engine, err := dependency.NewEngine(dependency.EngineConfig{
				IsFatal:       cmdutil.IsFatal,
				MoreImportant: func(_ error, worst error) error { return worst },
				ErrorDelay:    3 * time.Second,
				BounceDelay:   10 * time.Millisecond,
			})
			if err != nil {
				return nil, errors.Trace(err)
			}

			var runner worker.Runner            // TODO(ericsnow) Wrap engine in a runner.
			unitHandler.Init(apiClient, runner) // TODO(ericsnow) Eliminate this...

			manifolds := unitHandler.Manifolds()
			if err := dependency.Install(engine, manifolds); err != nil {
				if err := worker.Stop(engine); err != nil {
					workloadsLogger.Errorf("while stopping engine with bad manifolds: %v", err)
				}
				return nil, errors.Trace(err)
			}

			return engine, nil
		})
		return manifold, nil
	}
	err := unit.RegisterManifold(workload.ComponentName, newManifold)
	if err != nil {
		panic(err)
	}

	return unitEventHandlers
}

func (workloads) registerState() {
	// TODO(ericsnow) Use a more general registration mechanism.
	//state.RegisterMultiEnvCollections(persistence.Collections...)

	newUnitWorkloads := func(persist state.Persistence, unit names.UnitTag, getMetadata func() (*charm.Meta, error)) (state.UnitWorkloads, error) {
		return workloadstate.NewUnitWorkloads(persist, unit, getMetadata), nil
	}
	state.SetWorkloadsComponent(newUnitWorkloads)
}

func (workloads) registerUnitStatus() {
	apiserverclient.RegisterStatusProviderForUnits(workload.ComponentName,
		func(unit *state.Unit) (interface{}, error) {
			up, err := unit.Workloads()
			if err != nil {
				return nil, err
			}
			workloads, err := up.List()
			if err != nil {
				return nil, err
			}
			return status.UnitStatus(workloads)
		})
}
