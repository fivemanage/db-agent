package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fivemanage/db-agent/internal/agent"
	manager "github.com/kardianos/service"
)

type fakeHost struct {
	program        manager.Interface
	cfg            *manager.Config
	platform       string
	stop           chan struct{}
	once           sync.Once
	hostErr        error
	transientStops int32
	stopCalls      atomic.Int32
}

func (s *fakeHost) Run() error {
	if err := s.program.Start(s); err != nil {
		return err
	}
	if s.hostErr != nil {
		return s.hostErr
	}
	if s.platform == "windows-service" {
		<-s.stop
		return nil
	}
	s.cfg.Option["RunWait"].(func())()
	return s.program.Stop(s)
}
func (s *fakeHost) Stop() error {
	if s.stopCalls.Add(1) <= s.transientStops {
		return errors.New("SCM is still starting the service")
	}
	err := s.program.Stop(s)
	s.once.Do(func() { close(s.stop) })
	return err
}
func (s *fakeHost) Platform() string                { return s.platform }
func (s *fakeHost) Start() error                    { return nil }
func (s *fakeHost) Install() error                  { return nil }
func (s *fakeHost) Uninstall() error                { return nil }
func (s *fakeHost) Restart() error                  { return nil }
func (s *fakeHost) String() string                  { return Name }
func (s *fakeHost) Status() (manager.Status, error) { return manager.StatusRunning, nil }
func (s *fakeHost) Logger(chan<- error) (manager.Logger, error) {
	return manager.ConsoleLogger, nil
}
func (s *fakeHost) SystemLogger(chan<- error) (manager.Logger, error) {
	return manager.ConsoleLogger, nil
}

func hostFactory(platform string, hostErr error) func(manager.Interface, *manager.Config) (manager.Service, error) {
	return func(program manager.Interface, cfg *manager.Config) (manager.Service, error) {
		return &fakeHost{program: program, cfg: cfg, platform: platform, stop: make(chan struct{}), hostErr: hostErr}, nil
	}
}

func TestServiceHostExitsWhenWorkFinishes(t *testing.T) {
	for _, platform := range []string{"linux-systemd", "darwin-launchd", "windows-service"} {
		for _, failure := range []bool{false, true} {
			t.Run(platform+map[bool]string{true: "/failure", false: "/update-handoff"}[failure], func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				workerErr := errors.New("credential rejected")
				cleaned := false
				cfg := &manager.Config{Name: Name}
				err := run(ctx, cfg, func(context.Context, manager.Service) error {
					if failure {
						return workerErr
					}
					return nil
				}, hostFactory(platform, nil), func(context.Context) error { cleaned = true; return nil })
				if (err != nil) != failure || (failure && !errors.Is(err, workerErr)) {
					t.Fatalf("worker result was lost: %v", err)
				}
				if !cleaned {
					t.Fatal("host exited before cleanup")
				}
				if cfg.Option != nil {
					t.Fatal("host mutated the caller's service config")
				}
			})
		}
	}
}

func TestServiceParentCancellationJoinsWorker(t *testing.T) {
	for _, platform := range []string{"linux-systemd", "darwin-launchd", "windows-service"} {
		t.Run(platform, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started, stopped := make(chan struct{}), make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- run(ctx, &manager.Config{Name: Name}, func(ctx context.Context, _ manager.Service) error {
					close(started)
					<-ctx.Done()
					close(stopped)
					return ctx.Err()
				}, hostFactory(platform, nil))
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("service worker did not start")
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("service ignored cancellation")
			}
			select {
			case <-stopped:
			default:
				t.Fatal("service worker leaked")
			}
		})
	}
}

func TestServiceHostFailureCancelsItsWorkers(t *testing.T) {
	hostErr := errors.New("service host failed")
	stopped := make(chan struct{})
	err := run(context.Background(), &manager.Config{Name: Name}, func(ctx context.Context, _ manager.Service) error {
		<-ctx.Done()
		close(stopped)
		return nil
	}, hostFactory("linux-systemd", hostErr))
	if !errors.Is(err, hostErr) {
		t.Fatalf("host failure lost: %v", err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("host failure leaked its worker")
	}
}

func TestWindowsFastWorkerExitRetriesStopDuringStartPending(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var host *fakeHost
	err := run(ctx, &manager.Config{Name: Name}, func(context.Context, manager.Service) error { return nil }, func(program manager.Interface, cfg *manager.Config) (manager.Service, error) {
		host = &fakeHost{program: program, cfg: cfg, platform: "windows-service", stop: make(chan struct{}), transientStops: 1}
		return host, nil
	})
	if err != nil || host.stopCalls.Load() != 2 {
		t.Fatalf("startup race left service alive: stops=%d err=%v", host.stopCalls.Load(), err)
	}
}

func TestServiceFactoryFailureDoesNotStartWorker(t *testing.T) {
	failure := errors.New("service manager unavailable")
	err := run(context.Background(), &manager.Config{Name: Name}, func(context.Context, manager.Service) error {
		t.Error("worker started without a service host")
		return nil
	}, func(manager.Interface, *manager.Config) (manager.Service, error) { return nil, failure })
	if !errors.Is(err, failure) {
		t.Fatalf("service-manager failure lost: %v", err)
	}
}

func TestWindowsMachineShutdownUsesSameCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	joined := false
	p := &program{ctx: ctx, runtime: agent.New(func(ctx context.Context) error {
		<-ctx.Done()
		joined = true
		return nil
	})}
	if err := p.runtime.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.Shutdown(nil); err != nil || !joined {
		t.Fatalf("machine shutdown did not join work: %v", err)
	}
}
