package testing

import (
	"sync"

	"github.com/sarthak/vessel/internal/routes"
)

// MockSendCall records a single invocation of Send on MockEmailProvider.
type MockSendCall struct {
	To          string
	Subject     string
	BodyHTML    string
	Attachments []routes.Attachment
}

// MockEmailProvider is a thread-safe test spy that implements EmailProvider.
//
// It records every Send call so tests can assert on delivery parameters
// without actually sending email over the wire.
type MockEmailProvider struct {
	mu sync.Mutex

	// Calls is an ordered log of every Send invocation.
	Calls []MockSendCall

	// SendError, when non-nil, is returned by every Send call. Use this to
	// simulate SMTP timeouts, API failures, etc. for retry-branch coverage.
	SendError error
}

func (m *MockEmailProvider) Send(to, subject, bodyHTML string, attachments []routes.Attachment) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.Calls = append(m.Calls, MockSendCall{
		To:          to,
		Subject:     subject,
		BodyHTML:    bodyHTML,
		Attachments: attachments,
	})

	if m.SendError != nil {
		return m.SendError
	}
	return nil
}

// Reset clears all recorded calls and the injected error.
// Call this between test cases to get a clean slate.
func (m *MockEmailProvider) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Calls = nil
	m.SendError = nil
}

// CallCount returns how many times Send was invoked.
func (m *MockEmailProvider) CallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.Calls)
}
