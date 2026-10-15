package limiter

import (
	"sync"
)

// AdaptiveSetting is a struct that holds the configuration parameters for an adaptive limiter.
type AdaptiveSetting struct {
	Initial       int
	Max           int
	Min           int
	BackoffFactor float64
}

// AfterUpdateHook is a function hook that is triggered when the current limit changes. The callers need to register a hook to
// the AdaptiveLimiter implementation beforehand. They are required to handle errors inside the hook function.
type AfterUpdateHook func(newVal int)

// AdaptiveLimiter is an interface for managing and updating adaptive limits.
// It exposes methods to get the name, current limit value, update the limit value, and access its settings.
type AdaptiveLimiter interface {
	Name() string
	Current() int
	Update(val int)
	AfterUpdate(AfterUpdateHook)
	Setting() AdaptiveSetting
}

// AdaptiveLimit is an implementation of the AdaptiveLimiter interface. It uses a mutex to ensure thread-safe access to the limit value.
type AdaptiveLimit struct {
	sync.Mutex

	name        string
	current     int
	setting     AdaptiveSetting
	updateHooks []AfterUpdateHook
}

// NewAdaptiveLimit initializes a new AdaptiveLimit object
func NewAdaptiveLimit(name string, setting AdaptiveSetting) *AdaptiveLimit {
	return &AdaptiveLimit{
		name:    name,
		current: setting.Initial,
		setting: setting,
	}
}

// Name returns the name of the adaptive limit
func (l *AdaptiveLimit) Name() string {
	return l.name
}

// Current returns the current limit. This function can be called without the need for synchronization.
func (l *AdaptiveLimit) Current() int {
	l.Lock()
	defer l.Unlock()

	return l.current
}

// Initial returns the initial limit.
func (l *AdaptiveLimit) Initial() int {
	return l.setting.Initial
}

// Update adjusts the current limit value and executes all registered update hooks.
func (l *AdaptiveLimit) Update(val int) {
	l.Lock()
	defer l.Unlock()

	if val != l.current {
		l.current = val
		for _, hook := range l.updateHooks {
			hook(val)
		}
	}
}

// AfterUpdate registers a callback when the current limit is updated. Because all updates and hooks are synchronized,
// calling l.Current() inside the update hook in the same goroutine will cause deadlock. Hence, the update hook must
// use the newVal argument instead.
func (l *AdaptiveLimit) AfterUpdate(hook AfterUpdateHook) {
	l.updateHooks = append(l.updateHooks, hook)
}

// Setting returns the configuration parameters for an adaptive limiter.
func (l *AdaptiveLimit) Setting() AdaptiveSetting {
	return l.setting
}
