package muse

import (
	"context"
	"errors"
	"time"
)

// engine runs one muse.ai web session per call in the remote browser. Muse has
// no public API: chat, images, videos and the usage panel all go through the
// web app, which talks to the account's cloud VM over an encrypted WebSocket.
type engine interface {
	// Chat sends one prompt in a new side chat and reports the reply as
	// Markdown snapshots while it renders; it returns the final Markdown.
	Chat(ctx context.Context, s session, turn chatTurn, onText func(markdown string) error) (string, error)
	// Generate asks the agent for one image or video and returns its bytes.
	Generate(ctx context.Context, s session, job mediaJob) (mediaFile, error)
	// Usage reads Settings → General → Usage.
	Usage(ctx context.Context, s session) (usage, error)
}

// session is the account a browser context logs in as.
type session struct {
	Cookies []cookie
	// ProxyServer is scheme://host:port without credentials; empty is direct.
	ProxyServer string
	// Identity is the desktop browser the account presents; zero keeps the
	// browser's own.
	Identity browserIdentity
	// Timezone is the IANA zone the page runs in; empty keeps the browser's.
	Timezone string
}

// cookie uses the Cookie-Editor export field names, so an exported file is a
// stored credential as is.
type cookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain,omitempty"`
	Path     string  `json:"path,omitempty"`
	Expires  float64 `json:"expirationDate,omitempty"` // unix seconds; 0 is a session cookie
	HostOnly bool    `json:"hostOnly,omitempty"`
	HTTPOnly bool    `json:"httpOnly,omitempty"`
	Secure   bool    `json:"secure,omitempty"`
	SameSite string  `json:"sameSite,omitempty"` // lax, strict, no_restriction or unspecified
}

// attachment is a file put into the composer before the prompt is sent.
type attachment struct {
	Name string
	MIME string
	Data []byte
}

type chatTurn struct {
	Prompt string
	Images []attachment
}

type mediaKind string

const (
	mediaImage mediaKind = "image"
	mediaVideo mediaKind = "video"
)

type mediaJob struct {
	Kind   mediaKind
	Prompt string
	Inputs []attachment
	// Thread resumes a job whose prompt was already sent: the engine opens
	// this thread path (/thread/<id>) and only waits for its result.
	Thread string
	// OnThread receives the thread path once the prompt is sent, so a restart
	// can resume the job instead of sending it again.
	OnThread func(path string) error
}

type mediaFile struct {
	Data []byte
	MIME string
	// Width and Height are 0 when the browser cannot decode the file; the
	// headless shell has no H.264, so videos always report 0.
	Width, Height int
}

type usage struct {
	Plan        string    // "Free plan"
	UsedPercent float64   // weekly limit used, 0–100
	ResetAt     time.Time // zero when the panel shows no reset date
}

// Engine errors. The adapter maps them to gateway statuses.
var (
	errBrowserUnavailable = errors.New("muse browser unavailable")  // CDP endpoint unreachable: request-scoped 503
	errLoggedOut          = errors.New("muse session logged out")   // cookies rejected: 401, account needs re-import
	errQuotaExhausted     = errors.New("muse usage limit reached")  // 429 until the weekly reset
	errNotDelivered       = errors.New("muse prompt not delivered") // composer never accepted or sent the prompt: 502
	errTimeout            = errors.New("muse reply timed out")      // no reply or result in time: 504
)

// textOnlyError means the agent answered in text where a file was expected,
// usually a refusal. It is the request's fault, not the account's.
type textOnlyError struct{ Text string }

func (e *textOnlyError) Error() string { return "muse replied without a file: " + e.Text }

// Config holds the runtime settings the adapter reads on every call.
type Config struct {
	// BrowserURL is the DevTools HTTP endpoint of the shared headless Chrome;
	// empty means http://muse-browser:9222 (use browserURL()).
	BrowserURL string
	// MaxPages caps concurrent browser pages; 0 means 2.
	MaxPages int
}

// DefaultBrowserURL is the browser service of the bundled docker-compose.
const DefaultBrowserURL = "http://muse-browser:9222"

func (c Config) browserURL() string {
	if c.BrowserURL != "" {
		return c.BrowserURL
	}
	return DefaultBrowserURL
}

func (c Config) maxPages() int {
	if c.MaxPages > 0 {
		return c.MaxPages
	}
	return 2
}
