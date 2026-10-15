package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// maxSlackRequestBytes bounds a request from Slack and the replies read back.
	maxSlackRequestBytes = 256 << 10
	defaultConcurrent    = 4
	// asyncTimeout bounds one background answer and its reply.
	asyncTimeout    = 10 * time.Minute
	slackAPITimeout = 10 * time.Second
	// eventDedupTTL is how long an event_id is remembered to drop the
	// duplicate deliveries of an event. Slack retries within minutes.
	eventDedupTTL = 10 * time.Minute
	// maxSeenEvents bounds the remembered event_ids; the oldest is forgotten
	// first.
	maxSeenEvents = 10000

	usageText  = "Usage: /graft <question>, for example /graft what changed in the last hour?"
	busyText   = "graft-superfan is busy answering other questions. Try again in a minute."
	failedText = "graft-superfan could not answer this question. The server log has the details."
	tooLongMsg = "Please ask a shorter question."
)

// ErrNoBotToken means an app_mention could not be answered in the channel.
var ErrNoBotToken = errors.New("chat: no Slack bot token configured")

// mentionPrefix matches the bot mention that starts an app_mention text.
var mentionPrefix = regexp.MustCompile(`^\s*<@[A-Za-z0-9]+>\s*`)

// slackUnescaper undoes the escaping Slack applies to event text.
var slackUnescaper = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&")

// slackEscaper makes model text safe for mrkdwn: it cannot ping channels or
// inject links.
var slackEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// ServerOptions configures the Slack front end.
type ServerOptions struct {
	// Engine answers the questions.
	Engine *Engine
	// SigningSecret verifies the requests. It is required.
	SigningSecret string
	// BotToken authenticates chat.postMessage. Empty disables app_mention
	// replies (they are logged and dropped).
	BotToken string
	// APIBase is the Slack Web API base URL; empty means https://slack.com/api.
	APIBase string
	// HTTPClient calls Slack and the response URLs; nil means a client with a
	// slackAPITimeout timeout.
	HTTPClient *http.Client
	// Now returns the current time; nil means time.Now.
	Now func() time.Time
	// Logger receives operational messages; nil discards them. Questions and
	// answers are never logged, and neither are secrets.
	Logger *slog.Logger
	// MaxConcurrent bounds the answers computed at the same time; zero means 4.
	// A request over the bound is answered with a busy message.
	MaxConcurrent int
}

// Server answers Slack slash commands and events. Answers are computed in the
// background; the request is acknowledged within Slack's 3 second window.
type Server struct {
	opts    ServerOptions
	client  *http.Client
	apiBase string
	sem     chan struct{}
	tasks   sync.WaitGroup

	// seenMu guards seen, the event_ids delivered in the last eventDedupTTL
	// and the time they were first received.
	seenMu sync.Mutex
	seen   map[string]time.Time
}

// NewServer builds the Slack front end.
func NewServer(opts ServerOptions) (*Server, error) {
	if opts.Engine == nil {
		return nil, errors.New("chat: the server needs an engine")
	}
	if opts.SigningSecret == "" {
		return nil, ErrNoSecret
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = defaultConcurrent
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: slackAPITimeout}
	}
	apiBase := strings.TrimRight(opts.APIBase, "/")
	if apiBase == "" {
		apiBase = defaultSlackAPIBase
	}
	return &Server{
		opts:    opts,
		client:  client,
		apiBase: apiBase,
		sem:     make(chan struct{}, opts.MaxConcurrent),
		seen:    make(map[string]time.Time),
	}, nil
}

// Handler serves POST /slack/commands (slash commands) and POST /slack/events
// (Events API).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /slack/commands", s.handleCommand)
	mux.HandleFunc("POST /slack/events", s.handleEvents)
	return mux
}

// Wait blocks until the background answers started so far have finished.
func (s *Server) Wait() {
	s.tasks.Wait()
}

// Drain waits at most timeout for the background answers started so far. It
// reports whether they all finished. Call it after Serve returns, so that an
// answer already acknowledged to Slack is not abandoned at shutdown.
func (s *Server) Drain(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		s.tasks.Wait()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// readVerified reads the body of a Slack request and checks its signature.
// It writes the error response itself and reports false when the request must
// not be processed.
func (s *Server) readVerified(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSlackRequestBytes))
	if err != nil {
		http.Error(w, "request too large or unreadable", http.StatusRequestEntityTooLarge)
		return nil, false
	}
	if err := VerifySlack(s.opts.SigningSecret, r.Header, body, s.opts.Now()); err != nil {
		s.opts.Logger.Warn("rejected Slack request", "path", r.URL.Path, "reason", err.Error())
		http.Error(w, "invalid request signature", http.StatusUnauthorized)
		return nil, false
	}
	return body, true
}

// handleCommand answers /graft <question>. The question is answered to the
// response_url, as an ephemeral message.
func (s *Server) handleCommand(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readVerified(w, r)
	if !ok {
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	question := strings.TrimSpace(form.Get("text"))
	if question == "" {
		writeEphemeral(w, usageText)
		return
	}
	responseURL := form.Get("response_url")
	if err := validHTTPURL(responseURL); err != nil {
		http.Error(w, "missing response_url", http.StatusBadRequest)
		return
	}
	if len(question) > maxQuestionBytes {
		writeEphemeral(w, tooLongMsg)
		return
	}
	started := s.async(func(ctx context.Context) {
		text := s.answerText(ctx, question)
		if err := s.postResponse(ctx, responseURL, text); err != nil {
			s.opts.Logger.Error("post slash command answer", "err", err)
		}
	})
	if !started {
		writeEphemeral(w, busyText)
		return
	}
	w.WriteHeader(http.StatusOK)
}

type slackEnvelope struct {
	Type      string      `json:"type"`
	Challenge string      `json:"challenge"`
	EventID   string      `json:"event_id"`
	Event     *slackEvent `json:"event"`
}

type slackEvent struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Channel  string `json:"channel"`
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts"`
	BotID    string `json:"bot_id"`
}

// handleEvents answers the URL verification challenge and app_mention events.
// Other events are acknowledged and ignored.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readVerified(w, r)
	if !ok {
		return
	}
	var env slackEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		http.Error(w, "malformed event", http.StatusBadRequest)
		return
	}
	switch env.Type {
	case "url_verification":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, env.Challenge)
	case "event_callback":
		s.handleAppMention(w, r, env.EventID, env.Event)
	default:
		w.WriteHeader(http.StatusOK)
	}
}

// handleAppMention answers an app_mention in a thread. Messages from bots
// (including this one) are acknowledged and not answered, so that the answers
// cannot loop.
//
// Duplicates are dropped by event_id: a Slack retry is answered only when its
// event_id has not been received in the last eventDedupTTL, because Slack also
// retries when the first delivery never reached this server (connection
// failure, restart, proxy error). An envelope without an event_id cannot be
// deduplicated, so its retries are dropped.
func (s *Server) handleAppMention(w http.ResponseWriter, r *http.Request, eventID string, ev *slackEvent) {
	defer w.WriteHeader(http.StatusOK)
	if ev == nil || ev.Type != "app_mention" || ev.BotID != "" {
		return
	}
	if eventID == "" {
		if r.Header.Get(slackRetryHeader) != "" {
			return
		}
	} else if !s.firstDelivery(eventID) {
		return
	}
	question := strings.TrimSpace(slackUnescaper.Replace(mentionPrefix.ReplaceAllString(ev.Text, "")))
	if question == "" || len(question) > maxQuestionBytes || ev.Channel == "" {
		return
	}
	thread := ev.ThreadTS
	if thread == "" {
		thread = ev.TS
	}
	if !s.async(func(ctx context.Context) {
		text := s.answerText(ctx, question)
		if err := s.postMessage(ctx, ev.Channel, thread, text); err != nil {
			s.opts.Logger.Error("post app_mention answer", "err", err)
		}
	}) {
		s.opts.Logger.Warn("dropped app_mention: too many answers in progress")
	}
}

// firstDelivery records eventID and reports whether it was not received in the
// last eventDedupTTL. When maxSeenEvents ids are remembered, the expired ones
// are forgotten and, if none expired, the oldest one.
func (s *Server) firstDelivery(eventID string) bool {
	now := s.opts.Now()
	s.seenMu.Lock()
	defer s.seenMu.Unlock()
	if at, ok := s.seen[eventID]; ok && seenRecently(at, now) {
		return false
	}
	if len(s.seen) >= maxSeenEvents {
		for id, at := range s.seen {
			if !seenRecently(at, now) {
				delete(s.seen, id)
			}
		}
	}
	if len(s.seen) >= maxSeenEvents {
		oldestID, oldest := "", time.Time{}
		for id, at := range s.seen {
			if oldestID == "" || at.Before(oldest) {
				oldestID, oldest = id, at
			}
		}
		delete(s.seen, oldestID)
	}
	s.seen[eventID] = now
	return true
}

// seenRecently reports whether an event received at was received less than
// eventDedupTTL before now. A time after now (the clock went back) counts as
// recent.
func seenRecently(at, now time.Time) bool {
	return now.Sub(at) < eventDedupTTL
}

// answerText asks the engine and returns the escaped text to post.
func (s *Server) answerText(ctx context.Context, question string) string {
	answer, err := s.opts.Engine.Answer(ctx, question)
	if err != nil {
		s.opts.Logger.Error("answer question", "err", err)
		return failedText
	}
	return slackEscaper.Replace(answer)
}

// async runs fn in the background, bounded by MaxConcurrent. It reports false
// without running fn when the bound is reached.
func (s *Server) async(fn func(ctx context.Context)) bool {
	select {
	case s.sem <- struct{}{}:
	default:
		return false
	}
	s.tasks.Add(1)
	go func() {
		defer s.tasks.Done()
		defer func() { <-s.sem }()
		ctx, cancel := context.WithTimeout(context.Background(), asyncTimeout)
		defer cancel()
		fn(ctx)
	}()
	return true
}

// writeEphemeral acknowledges a request with a message only the sender sees.
func writeEphemeral(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]string{"response_type": "ephemeral", "text": text})
}

// postResponse posts an ephemeral message to a slash command's response_url.
func (s *Server) postResponse(ctx context.Context, responseURL, text string) error {
	data, err := json.Marshal(map[string]string{"response_type": "ephemeral", "text": text})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, responseURL, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("chat: response_url: %w", safeErr(err))
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("chat: post response: %w", safeErr(err))
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxSlackRequestBytes))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("chat: post response: HTTP %d", resp.StatusCode)
	}
	return nil
}

// postMessage posts text in a thread with chat.postMessage, authenticated by
// the bot token.
func (s *Server) postMessage(ctx context.Context, channel, threadTS, text string) error {
	if s.opts.BotToken == "" {
		return ErrNoBotToken
	}
	payload := map[string]string{"channel": channel, "text": text}
	if threadTS != "" {
		payload["thread_ts"] = threadTS
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.apiBase+"/chat.postMessage", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("chat: chat.postMessage: %w", safeErr(err))
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+s.opts.BotToken)
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("chat: chat.postMessage: %w", safeErr(err))
	}
	defer resp.Body.Close()
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxSlackRequestBytes)).Decode(&out); err != nil {
		return fmt.Errorf("chat: chat.postMessage: HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode/100 != 2 || !out.OK {
		return fmt.Errorf("chat: chat.postMessage failed: HTTP %d, %s", resp.StatusCode, out.Error)
	}
	return nil
}

// Serve runs h on listen until ctx is done, then shuts the server down.
func Serve(ctx context.Context, listen string, h http.Handler) error {
	srv := &http.Server{
		Addr:              listen,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       time.Minute,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}
