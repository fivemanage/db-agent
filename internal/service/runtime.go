package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fivemanage/db-agent/internal/agent"
	manager "github.com/kardianos/service"
)

type program struct {
	ctx        context.Context
	runtime    *agent.Runtime
	stopping   atomic.Bool
	stopErr    chan error
	stopOnce   sync.Once
	stopResult error
}

func (p *program) Start(svc manager.Service) error {
	if err := p.runtime.Start(p.ctx); err != nil {
		return err
	}

	if svc.Platform() == "windows-service" {
		// Windows RunWait is not supported. Ask SCM to deliver its normal stop
		// callback on a worker failure or update handoff, not just on user stop.
		go func() {
			select {
			case <-p.runtime.Done():
			case <-p.ctx.Done():
			}

			if !p.stopping.Load() {
				if err := p.requestStop(svc); err != nil {
					p.stopErr <- err
				}
			}
		}()
	}

	return nil
}

func (p *program) requestStop(svc manager.Service) error {
	// A fast startup failure can precede SCM's Running transition. Retry the
	// stop request briefly rather than leaving a host with no live workers.
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		err := svc.Stop()
		if err == nil || p.stopping.Load() {
			return nil
		}

		select {
		case <-deadline.C:
			return fmt.Errorf("request service shutdown: %w", err)
		case <-ticker.C:
		}
	}
}

func (p *program) Stop(manager.Service) error {
	p.stopping.Store(true)
	p.stopOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), agent.ShutdownTimeout)
		defer cancel()

		if err := p.runtime.Stop(ctx); err != nil {
			p.stopResult = fmt.Errorf("agent shutdown: %w", err)
		}
	})

	return p.stopResult
}

func (p *program) Shutdown(svc manager.Service) error { return p.Stop(svc) }

func Run(ctx context.Context, cfg *manager.Config, work func(context.Context, manager.Service) error, hooks ...agent.Hook) error {
	return run(ctx, cfg, work, New, hooks...)
}

func run(ctx context.Context, cfg *manager.Config, work func(context.Context, manager.Service) error, factory func(manager.Interface, *manager.Config) (manager.Service, error), hooks ...agent.Hook) error {
	var svc manager.Service
	p := &program{ctx: ctx, stopErr: make(chan error, 1)}
	p.runtime = agent.New(func(ctx context.Context) error { return work(ctx, svc) }, hooks...)

	// RunWait makes Unix service hosts return on worker exit as well as SIGTERM.
	options := make(manager.KeyValue, len(cfg.Option)+1)
	for key, value := range cfg.Option {
		options[key] = value
	}
	options["RunWait"] = func() {
		select {
		case <-ctx.Done():
		case <-p.runtime.Done():
		}
	}

	copy := *cfg
	copy.Option = options
	var err error
	svc, err = factory(p, &copy)
	if err != nil {
		return err
	}

	hostDone := make(chan error, 1)
	go func() { hostDone <- svc.Run() }()
	select {
	case err = <-hostDone:
	case err = <-p.stopErr:
	}

	// Also clean up if the service host exits without invoking Stop.
	stopErr := p.Stop(svc)
	if err == nil {
		return stopErr
	}

	if stopErr == nil || errors.Is(err, stopErr) {
		return err
	}

	return errors.Join(err, stopErr)
}
