package auth

import (
	"errors"
	"sync"
)

// FallbackViewFactory moves the same backend login session to a fresh browser
// view, never copying cookies. embedded receives requestBrowser, which the
// embedded view calls when the user asks to continue in Chrome.
func FallbackViewFactory(embedded func(requestBrowser func() error) ViewFactory, browser func() (ViewFactory, error)) ViewFactory {
	return func(callbacks Callbacks) View {
		f := &fallbackView{callbacks: callbacks, browser: browser}
		f.view = f.create(embedded(f.requestBrowser))
		return f
	}
}

type fallbackView struct {
	callbacks Callbacks
	browser   func() (ViewFactory, error)

	mu           sync.Mutex
	view         View
	frame        *Frame
	disposed     bool
	generation   int
	usingBrowser bool
	switching    bool
	switched     chan struct{} // closed when the current switch finishes
}

// waitSwitch blocks while the view is being replaced, so events reach the new view.
func (f *fallbackView) waitSwitch() {
	f.mu.Lock()
	done := f.switched
	switching := f.switching
	f.mu.Unlock()
	if switching && done != nil {
		<-done
	}
}

func (f *fallbackView) create(factory ViewFactory) View {
	f.mu.Lock()
	own := f.generation
	f.mu.Unlock()
	current := func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return !f.disposed && f.generation == own
	}
	return factory(Callbacks{
		Send: func(id int64, payload string) {
			if current() {
				f.callbacks.Send(id, payload)
			}
		},
		Cancel: func(id int64) {
			if current() {
				f.callbacks.Cancel(id)
			}
		},
		Fail: func() {
			if current() {
				f.callbacks.Fail()
			}
		},
	})
}

func (f *fallbackView) requestBrowser() error {
	f.mu.Lock()
	if f.disposed || f.usingBrowser || f.switching || f.frame == nil {
		f.mu.Unlock()
		return nil
	}
	active, own := f.frame, f.generation
	f.mu.Unlock()
	// Resolve availability before closing the embedded window. Missing Chrome
	// leaves the working view and pending Microsoft request alone.
	factory, err := f.browser()
	if err != nil {
		return err
	}
	f.mu.Lock()
	if f.disposed || f.usingBrowser || f.switching || f.frame != active || f.generation != own {
		f.mu.Unlock()
		return nil
	}
	f.switching = true
	f.switched = make(chan struct{})
	done := f.switched
	defer close(done)
	f.generation++ // Invalidate all events from the old window before destroying it.
	old := f.view
	f.mu.Unlock()
	old.Dispose()
	f.mu.Lock()
	if f.disposed {
		f.switching = false
		f.mu.Unlock()
		return nil
	}
	f.usingBrowser = true
	f.mu.Unlock()
	next := f.create(factory)
	f.mu.Lock()
	f.view = next
	still := f.frame == active
	f.mu.Unlock()
	if still {
		err = next.Open(*active)
	}
	f.mu.Lock()
	f.switching = false
	f.mu.Unlock()
	if err != nil {
		f.callbacks.Fail()
		return errors.New("Browser sign-in could not be opened.")
	}
	return nil
}

func (f *fallbackView) Open(frame Frame) error {
	f.waitSwitch()
	f.mu.Lock()
	if f.disposed {
		f.mu.Unlock()
		return nil
	}
	f.frame = &frame
	own, current := f.generation, f.view
	f.mu.Unlock()
	if err := current.Open(frame); err != nil {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.disposed && f.generation == own {
			return err
		}
	}
	return nil
}

func (f *fallbackView) Close(session int64) {
	f.mu.Lock()
	if f.frame == nil || f.frame.Session != session {
		f.mu.Unlock()
		return
	}
	f.frame = nil
	current := f.view
	f.mu.Unlock()
	current.Close(session)
}

func (f *fallbackView) Context(session int64, context string) error {
	f.waitSwitch()
	f.mu.Lock()
	if f.disposed || f.frame == nil || f.frame.Session != session {
		f.mu.Unlock()
		return nil
	}
	own, current := f.generation, f.view
	f.mu.Unlock()
	if err := current.Context(session, context); err != nil {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.disposed && f.generation == own {
			return err
		}
	}
	return nil
}

func (f *fallbackView) Dispose() {
	f.mu.Lock()
	if f.disposed {
		f.mu.Unlock()
		return
	}
	f.disposed = true
	f.generation++
	f.frame = nil
	current := f.view
	f.mu.Unlock()
	current.Dispose()
}
