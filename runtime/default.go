package runtime

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hpcloud/tail"
	"github.com/micro/go-micro/v2/logger"
)

type runtime struct {
	sync.RWMutex
	// options configure runtime
	options Options
	// used to stop the runtime
	closed chan bool
	// used to start new services
	start chan *service
	// indicates if we're running
	running bool
	// the service map
	// TODO: track different versions of the same service
	services map[string]*service
}

// NewRuntime creates new local runtime and returns it
func NewRuntime(opts ...Option) Runtime {
	// get default options
	options := Options{}

	// apply requested options
	for _, o := range opts {
		o(&options)
	}

	return &runtime{
		options:  options,
		closed:   make(chan bool),
		start:    make(chan *service, 128),
		services: make(map[string]*service),
	}
}

// Init initializes runtime options
func (r *runtime) Init(opts ...Option) error {
	r.Lock()
	defer r.Unlock()

	for _, o := range opts {
		o(&r.options)
	}

	return nil
}

// run runs the runtime management loop
func (r *runtime) run(events <-chan Event) {
	t := time.NewTicker(time.Second * 5)
	defer t.Stop()

	// process event processes an incoming event
	processEvent := func(event Event, service *service) error {
		// get current vals
		r.RLock()
		name := service.Name
		updated := service.updated
		r.RUnlock()

		// only process if the timestamp is newer
		if !event.Timestamp.After(updated) {
			return nil
		}

		if logger.V(logger.DebugLevel, logger.DefaultLogger) {
			logger.Debugf("Runtime updating service %s", name)
		}

		// this will cause a delete followed by created
		if err := r.Update(service.Service); err != nil {
			return err
		}

		// update the local timestamp
		r.Lock()
		service.updated = updated
		r.Unlock()

		return nil
	}

	for {
		select {
		case <-t.C:
			// check running services
			r.RLock()
			for _, service := range r.services {
				if !service.ShouldStart() {
					continue
				}

				// TODO: check service error
				if logger.V(logger.DebugLevel, logger.DefaultLogger) {
					logger.Debugf("Runtime starting %s", service.Name)
				}
				if err := service.Start(); err != nil {
					if logger.V(logger.DebugLevel, logger.DefaultLogger) {
						logger.Debugf("Runtime error starting %s: %v", service.Name, err)
					}
				}
			}
			r.RUnlock()
		case service := <-r.start:
			if !service.ShouldStart() {
				continue
			}
			// TODO: check service error
			if logger.V(logger.DebugLevel, logger.DefaultLogger) {
				logger.Debugf("Runtime starting service %s", service.Name)
			}
			if err := service.Start(); err != nil {
				if logger.V(logger.DebugLevel, logger.DefaultLogger) {
					logger.Debugf("Runtime error starting service %s: %v", service.Name, err)
				}
			}
		case event := <-events:
			if logger.V(logger.DebugLevel, logger.DefaultLogger) {
				logger.Debugf("Runtime received notification event: %v", event)
			}
			// NOTE: we only handle Update events for now
			switch event.Type {
			case Update:
				if len(event.Service) > 0 {
					r.RLock()
					service, ok := r.services[event.Service]
					r.RUnlock()
					if !ok {
						if logger.V(logger.DebugLevel, logger.DefaultLogger) {
							logger.Debugf("Runtime unknown service: %s", event.Service)
						}
						continue
					}
					if err := processEvent(event, service); err != nil {
						if logger.V(logger.DebugLevel, logger.DefaultLogger) {
							logger.Debugf("Runtime error updating service %s: %v", event.Service, err)
						}
					}
					continue
				}

				r.RLock()
				services := r.services
				r.RUnlock()

				// if blank service was received we update all services
				for _, service := range services {
					if err := processEvent(event, service); err != nil {
						if logger.V(logger.DebugLevel, logger.DefaultLogger) {
							logger.Debugf("Runtime error updating service %s: %v", service.Name, err)
						}
					}
				}
			}
		case <-r.closed:
			if logger.V(logger.DebugLevel, logger.DefaultLogger) {
				logger.Debugf("Runtime stopped")
			}
			return
		}
	}
}

func logFile(serviceName string) string {
	name := strings.Replace(serviceName, "/", "-", -1)
	return filepath.Join(os.TempDir(), fmt.Sprintf("%v.log", name))
}

// Create creates a new service which is then started by runtime
func (r *runtime) Create(s *Service, opts ...CreateOption) error {
	r.Lock()
	defer r.Unlock()

	if _, ok := r.services[s.Name]; ok {
		return errors.New("service already running")
	}

	var options CreateOptions
	for _, o := range opts {
		o(&options)
	}

	if len(options.Command) == 0 {
		options.Command = []string{"go"}
		options.Args = []string{"run", "."}
	}

	// create new service
	service := newService(s, options)

	f, err := os.OpenFile(logFile(service.Name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	logger.Info(f, err)
	if err != nil {
		log.Fatal(err)
	}

	if service.output != nil {
		service.output = io.MultiWriter(service.output, f)
	} else {
		service.output = f
	}
	// start the service
	if err := service.Start(); err != nil {
		return err
	}

	// save service
	r.services[s.Name] = service

	return nil
}

// @todo: Getting existing lines is not supported yet.
// The reason for this is because it's hard to calculate line offset
// as opposed to character offset.
// This logger streams by default and only supports the `StreamCount` option.
func (r *runtime) Logs(s *Service, options ...LogsOption) (LogStream, error) {
	lopts := LogsOptions{}
	for _, o := range options {
		o(&lopts)
	}
	ret := &logStream{
		service: s.Name,
		stream:  make(chan LogRecord),
		stop:    make(chan bool),
	}
	t, err := tail.TailFile(logFile(s.Name), tail.Config{Follow: true, Location: &tail.SeekInfo{
		Whence: 2,
		Offset: 0,
	}, Logger: tail.DiscardingLogger})
	if err != nil {
		return nil, err
	}
	ret.tail = t
	go func() {
		for line := range t.Lines {
			ret.stream <- LogRecord{Message: line.Text}
		}
	}()
	return ret, nil
}

type logStream struct {
	tail    *tail.Tail
	service string
	stream  chan LogRecord
	stop    chan bool
	err     error
}

func (l *logStream) Chan() chan LogRecord {
	return l.stream
}

func (l *logStream) Error() error {
	return l.err
}

func (l *logStream) Stop() error {
	// @todo seems like this is causing a hangup
	//err := l.tail.Stop()
	//if err != nil {
	//	return err
	//}
	select {
	case <-l.stop:
		return nil
	default:
		close(l.stop)
	}
	return nil
}

// Read returns all instances of requested service
// If no service name is provided we return all the track services.
func (r *runtime) Read(opts ...ReadOption) ([]*Service, error) {
	r.Lock()
	defer r.Unlock()

	gopts := ReadOptions{}
	for _, o := range opts {
		o(&gopts)
	}

	save := func(k, v string) bool {
		if len(k) == 0 {
			return true
		}
		return k == v
	}

	//nolint:prealloc
	var services []*Service

	for _, service := range r.services {
		if !save(gopts.Service, service.Name) {
			continue
		}
		if !save(gopts.Version, service.Version) {
			continue
		}
		// TODO deal with service type
		// no version has sbeen requested, just append the service
		services = append(services, service.Service)
	}

	return services, nil
}

// Update attemps to update the service
func (r *runtime) Update(s *Service) error {
	var opts []CreateOption

	// check if the service already exists
	r.RLock()
	if service, ok := r.services[s.Name]; ok {
		opts = append(opts, WithOutput(service.output))
	}
	r.RUnlock()

	// delete the service
	if err := r.Delete(s); err != nil {
		return err
	}

	// create new service
	return r.Create(s, opts...)
}

// Delete removes the service from the runtime and stops it
func (r *runtime) Delete(s *Service) error {
	r.Lock()
	defer r.Unlock()

	if logger.V(logger.DebugLevel, logger.DefaultLogger) {
		logger.Debugf("Runtime deleting service %s", s.Name)
	}
	if s, ok := r.services[s.Name]; ok {
		// check if running
		if s.Running() {
			delete(r.services, s.Name)
			return nil
		}
		// otherwise stop it
		if err := s.Stop(); err != nil {
			return err
		}
		// delete it
		delete(r.services, s.Name)
		return nil
	}

	return nil
}

// List returns a slice of all services tracked by the runtime
func (r *runtime) List() ([]*Service, error) {
	r.RLock()
	defer r.RUnlock()

	services := make([]*Service, 0, len(r.services))

	for _, service := range r.services {
		services = append(services, service.Service)
	}

	return services, nil
}

// Start starts the runtime
func (r *runtime) Start() error {
	r.Lock()
	defer r.Unlock()

	// already running
	if r.running {
		return nil
	}

	// set running
	r.running = true
	r.closed = make(chan bool)

	var events <-chan Event
	if r.options.Scheduler != nil {
		var err error
		events, err = r.options.Scheduler.Notify()
		if err != nil {
			// TODO: should we bail here?
			if logger.V(logger.DebugLevel, logger.DefaultLogger) {
				logger.Debugf("Runtime failed to start update notifier")
			}
		}
	}

	go r.run(events)

	return nil
}

// Stop stops the runtime
func (r *runtime) Stop() error {
	r.Lock()
	defer r.Unlock()

	if !r.running {
		return nil
	}

	select {
	case <-r.closed:
		return nil
	default:
		close(r.closed)

		// set not running
		r.running = false

		// stop all the services
		for _, service := range r.services {
			if logger.V(logger.DebugLevel, logger.DefaultLogger) {
				logger.Debugf("Runtime stopping %s", service.Name)
			}
			service.Stop()
		}
		// stop the scheduler
		if r.options.Scheduler != nil {
			return r.options.Scheduler.Close()
		}
	}

	return nil
}

// String implements stringer interface
func (r *runtime) String() string {
	return "local"
}
