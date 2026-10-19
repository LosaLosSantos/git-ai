package config

import (
	"fmt"
	"path"
	"strings"
	"time"
)

// Graft configures the Graft extensions: the changes that turn Gitaly into a
// standalone, agent-scale, server-centric version control server (see
// doc/graft/PLAN.md). It is configured in the [graft] TOML section. Every
// field is optional; the zero value keeps upstream Gitaly behaviour except
// where noted.
type Graft struct {
	// Standalone runs Gitaly without GitLab: no GitLab secret or URL is
	// required, and access checks, pre-receive and post-receive callbacks are
	// answered by the built-in standalone client according to Policy instead
	// of GitLab's internal API.
	Standalone bool `json:"standalone" toml:"standalone,omitempty"`
	// Policy is the ordered list of access rules used in standalone mode. The
	// first rule whose Actor and Refs both match decides. When Policy is
	// empty every authenticated actor may update every reference.
	Policy []GraftPolicyRule `json:"policy" toml:"policy,omitempty"`
	// Agents are the per-agent credentials accepted by the gRPC server. When
	// at least one agent is configured, every request must present one of
	// these tokens (in addition to or instead of auth.token), and the agent
	// name becomes the request's identity.
	Agents []GraftAgent `json:"agents" toml:"agent,omitempty"`
	// ObjectService configures the ObjectService RPCs.
	ObjectService GraftObjectService `json:"object_service" toml:"object_service,omitempty"`
	// Changelog configures the changeset log.
	Changelog GraftChangelog `json:"changelog" toml:"changelog,omitempty"`
	// Derived configures derived-data caching.
	Derived GraftDerived `json:"derived" toml:"derived,omitempty"`
	// Land configures UserLand, notably the conflict resolver.
	Land GraftLand `json:"land" toml:"land,omitempty"`
	// AgentRPCs lists extra full gRPC method names (e.g.
	// "/gitaly.RepositoryService/RepositorySize") that callers authenticated
	// with an agent token may call in addition to the built-in agent
	// allowlist. Agents are untrusted: every other RPC is rejected with
	// PermissionDenied for them.
	AgentRPCs []string `json:"agent_rpcs" toml:"agent_rpcs,omitempty"`
	// Search configures the SearchService.
	Search GraftSearch `json:"search" toml:"search,omitempty"`
	// Review configures the ReviewService and its approval rules.
	Review GraftReview `json:"review" toml:"review,omitempty"`
	// Storage configures the storage core: repository state in a database
	// plus object storage (doc/graft/STORAGE.md).
	Storage GraftStorage `json:"storage" toml:"storage,omitempty"`
}

// GraftSearch configures code search.
type GraftSearch struct {
	// MaxResults caps the matches returned by one SearchCode call. Zero means
	// DefaultGraftSearchMaxResults.
	MaxResults int `json:"max_results" toml:"max_results,omitempty"`
	// MaxContextLines caps the context lines per match. Zero means
	// DefaultGraftSearchMaxContextLines.
	MaxContextLines int `json:"max_context_lines" toml:"max_context_lines,omitempty"`
	// Timeout bounds one search (Go duration). Empty means
	// DefaultGraftSearchTimeout.
	Timeout string `json:"timeout" toml:"timeout,omitempty"`
}

const (
	// DefaultGraftSearchMaxResults is the default GraftSearch.MaxResults.
	DefaultGraftSearchMaxResults = 1000
	// DefaultGraftSearchMaxContextLines is the default GraftSearch.MaxContextLines.
	DefaultGraftSearchMaxContextLines = 5
	// DefaultGraftSearchTimeout is the default GraftSearch.Timeout.
	DefaultGraftSearchTimeout = 10 * time.Second
)

// MaxResultsOrDefault returns MaxResults or its default.
func (s GraftSearch) MaxResultsOrDefault() int {
	if s.MaxResults > 0 {
		return s.MaxResults
	}
	return DefaultGraftSearchMaxResults
}

// MaxContextLinesOrDefault returns MaxContextLines or its default.
func (s GraftSearch) MaxContextLinesOrDefault() int {
	if s.MaxContextLines > 0 {
		return s.MaxContextLines
	}
	return DefaultGraftSearchMaxContextLines
}

// TimeoutOrDefault parses Timeout, applying the default.
func (s GraftSearch) TimeoutOrDefault() (time.Duration, error) {
	if s.Timeout == "" {
		return DefaultGraftSearchTimeout, nil
	}
	d, err := time.ParseDuration(s.Timeout)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("graft search: invalid timeout %q", s.Timeout)
	}
	return d, nil
}

// GraftReview configures code review.
type GraftReview struct {
	// Rules are the approval rules. Every rule applies to the changed files
	// matching its Paths and Classes; a review is approved when every rule
	// that applies to at least one file has enough current approvals. When
	// Rules is empty, DefaultGraftReviewRules apply.
	Rules []GraftReviewRule `json:"rules" toml:"rule,omitempty"`
	// MaxOpenReviews bounds the open reviews of one repository: CreateReview
	// refuses a review beyond it with ResourceExhausted. Every open review
	// keeps its head and base commits by references, in every mode, so the
	// bound also bounds those references. Zero means
	// DefaultGraftMaxOpenReviews.
	MaxOpenReviews int `json:"max_open_reviews" toml:"max_open_reviews,omitempty"`
}

// DefaultGraftMaxOpenReviews is the bound on the open reviews of a repository
// when max_open_reviews is not set.
const DefaultGraftMaxOpenReviews = 1000

// MaxOpenReviewsOrDefault returns MaxOpenReviews or DefaultGraftMaxOpenReviews.
func (r GraftReview) MaxOpenReviewsOrDefault() int {
	if r.MaxOpenReviews > 0 {
		return r.MaxOpenReviews
	}
	return DefaultGraftMaxOpenReviews
}

// GraftReviewRule is one approval rule.
type GraftReviewRule struct {
	// Name identifies the rule in review requirements.
	Name string `json:"name" toml:"name,omitempty"`
	// Paths are globs (policy ref glob syntax, "**" suffix) of files the rule
	// applies to; empty means every file.
	Paths []string `json:"paths" toml:"paths,omitempty"`
	// Classes are the change classes the rule applies to: "code",
	// "comment_only", "docs", "tests", "generated", "config"; empty means
	// every class.
	Classes []string `json:"classes" toml:"classes,omitempty"`
	// Approvals is the number of current approvals required.
	Approvals int `json:"approvals" toml:"approvals,omitempty"`
	// Approvers are globs of identities whose approvals count; empty means
	// everybody.
	Approvers []string `json:"approvers" toml:"approvers,omitempty"`
	// AllowAuthor lets the review author's own approval count.
	AllowAuthor bool `json:"allow_author" toml:"allow_author,omitempty"`
}

// GraftReviewClasses are the valid change class names.
var GraftReviewClasses = []string{"code", "comment_only", "docs", "tests", "generated", "config"}

// DefaultGraftReviewRules apply when no rule is configured: code and
// configuration changes need one approval from someone other than the
// author; comment-only, docs, tests and generated changes need none.
var DefaultGraftReviewRules = []GraftReviewRule{
	{Name: "code", Classes: []string{"code", "config"}, Approvals: 1},
}

// RulesOrDefault returns Rules or DefaultGraftReviewRules.
func (r GraftReview) RulesOrDefault() []GraftReviewRule {
	if len(r.Rules) > 0 {
		return r.Rules
	}
	return DefaultGraftReviewRules
}

func (r GraftReview) validate() error {
	if r.MaxOpenReviews < 0 {
		return fmt.Errorf("graft review: max_open_reviews must not be negative (0 means the default, %d)", DefaultGraftMaxOpenReviews)
	}
	for i, rule := range r.Rules {
		if rule.Name == "" {
			return fmt.Errorf("graft review rule %d: name is required", i+1)
		}
		if rule.Approvals < 0 {
			return fmt.Errorf("graft review rule %q: approvals must not be negative", rule.Name)
		}
		for _, class := range rule.Classes {
			valid := false
			for _, known := range GraftReviewClasses {
				valid = valid || class == known
			}
			if !valid {
				return fmt.Errorf("graft review rule %q: unknown class %q", rule.Name, class)
			}
		}
		for _, glob := range append(append([]string{}, rule.Paths...), rule.Approvers...) {
			if glob == "" {
				return fmt.Errorf("graft review rule %q: empty glob", rule.Name)
			}
			if _, err := path.Match(strings.TrimSuffix(glob, "**"), ""); err != nil {
				return fmt.Errorf("graft review rule %q: glob %q: %w", rule.Name, glob, err)
			}
		}
	}
	return nil
}

// GraftLand configures UserLand.
type GraftLand struct {
	// Resolver is the conflict resolver used when a UserLand request sets
	// resolve_conflicts. The server never talks to an LLM or any vendor API
	// itself: it calls this vendor-neutral resolver (see doc/graft/RESOLVE.md).
	Resolver GraftResolver `json:"resolver" toml:"resolver,omitempty"`
	// Batch configures batched landing (see doc/graft/LAND-BATCHING.md).
	Batch GraftLandBatch `json:"batch" toml:"batch,omitempty"`
	// GraphAncestry makes UserLand (single and batched) answer ancestry and
	// merge-base questions from the stored graph index instead of running git.
	GraphAncestry bool `json:"graph_ancestry" toml:"graph_ancestry,omitempty"`
}

// GraftLandBatch configures batched landing: concurrent UserLand requests on
// the same branch are grouped and published with a single reference update.
// Batching only applies in standalone mode.
type GraftLandBatch struct {
	// Disabled turns batching off; every UserLand request then runs its own
	// compare-and-swap retry loop.
	Disabled bool `json:"disabled" toml:"disabled,omitempty"`
	// Window is how long a lane collects requests after the first one (Go
	// duration). Empty means DefaultGraftLandBatchWindow; "0s" means no wait.
	Window string `json:"window" toml:"window,omitempty"`
	// MaxSize bounds the number of requests per batch. Zero means
	// DefaultGraftLandBatchMaxSize.
	MaxSize int `json:"max_size" toml:"max_size,omitempty"`
	// TargetLatency is the batch duration the lanes aim for (Go duration):
	// a lane shrinks its effective batch size after slower batches and grows
	// it after faster ones, up to MaxSize. Empty means
	// DefaultGraftLandBatchTargetLatency; "0s" disables the adaptation.
	TargetLatency string `json:"target_latency" toml:"target_latency,omitempty"`
}

// TargetLatencyOrDefault parses TargetLatency, applying the default.
func (b GraftLandBatch) TargetLatencyOrDefault() (time.Duration, error) {
	if b.TargetLatency == "" {
		return DefaultGraftLandBatchTargetLatency, nil
	}
	d, err := time.ParseDuration(b.TargetLatency)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("graft land batch: invalid target_latency %q", b.TargetLatency)
	}
	return d, nil
}

const (
	// DefaultGraftLandBatchTargetLatency is the default GraftLandBatch.TargetLatency.
	DefaultGraftLandBatchTargetLatency = 500 * time.Millisecond
	// DefaultGraftLandBatchWindow is the default GraftLandBatch.Window.
	DefaultGraftLandBatchWindow = 10 * time.Millisecond
	// DefaultGraftLandBatchMaxSize is the default GraftLandBatch.MaxSize.
	DefaultGraftLandBatchMaxSize = 50
	// MaxGraftLandBatchMaxSize is the largest GraftLandBatch.MaxSize. The update of a batch
	// records the identity of every member (doc/graft/PLAN11.md) in the hooks payload, which
	// bounds them (changelog.MaxLandsPerUpdate).
	MaxGraftLandBatchMaxSize = 100
)

// WindowOrDefault parses Window, applying the default.
func (b GraftLandBatch) WindowOrDefault() (time.Duration, error) {
	if b.Window == "" {
		return DefaultGraftLandBatchWindow, nil
	}
	d, err := time.ParseDuration(b.Window)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("graft land batch: invalid window %q", b.Window)
	}
	return d, nil
}

// MaxSizeOrDefault returns MaxSize or its default.
func (b GraftLandBatch) MaxSizeOrDefault() int {
	if b.MaxSize > 0 {
		return b.MaxSize
	}
	return DefaultGraftLandBatchMaxSize
}

func (b GraftLandBatch) validate() error {
	if _, err := b.WindowOrDefault(); err != nil {
		return err
	}
	if b.MaxSize < 0 {
		return fmt.Errorf("graft land batch: max_size must not be negative")
	}
	if b.MaxSize > MaxGraftLandBatchMaxSize {
		return fmt.Errorf("graft land batch: max_size must not exceed %d", MaxGraftLandBatchMaxSize)
	}
	if _, err := b.TargetLatencyOrDefault(); err != nil {
		return err
	}
	return nil
}

// GraftResolver configures the conflict resolver. Exactly one of Command and
// URL must be set when Enabled is true.
type GraftResolver struct {
	// Enabled turns automatic conflict resolution on. Off by default.
	Enabled bool `json:"enabled" toml:"enabled,omitempty"`
	// Command is an executable (with optional arguments) that reads one
	// resolve request as JSON on stdin and writes the response on stdout.
	Command []string `json:"command" toml:"command,omitempty"`
	// URL is an HTTP(S) endpoint that accepts the resolve request as a JSON
	// POST and answers with the response JSON.
	URL string `json:"url" toml:"url,omitempty"`
	// TokenFile, for URL resolvers, holds a bearer token sent in the
	// Authorization header.
	TokenFile string `json:"token_file" toml:"token_file,omitempty"`
	// Timeout bounds one resolver call, as a Go duration ("120s"). Empty means
	// DefaultGraftResolverTimeout.
	Timeout string `json:"timeout" toml:"timeout,omitempty"`
	// MaxFiles bounds the number of conflicting files sent in one request; a
	// conflict touching more files is not sent. Zero means
	// DefaultGraftResolverMaxFiles.
	MaxFiles int `json:"max_files" toml:"max_files,omitempty"`
	// MaxFileBytes bounds the size of every version of every file sent and of
	// every resolved file accepted. Zero means DefaultGraftResolverMaxFileBytes.
	MaxFileBytes int64 `json:"max_file_bytes" toml:"max_file_bytes,omitempty"`
	// PathsAllow are globs (same syntax as policy refs) of paths that may be
	// sent to the resolver; empty means every path.
	PathsAllow []string `json:"paths_allow" toml:"paths_allow,omitempty"`
	// PathsDeny are globs of paths that are never sent to the resolver (e.g.
	// secrets); a conflict touching such a path is not resolved
	// automatically. Deny wins over allow.
	PathsDeny []string `json:"paths_deny" toml:"paths_deny,omitempty"`
	// Actors are globs of identities allowed to request resolution; empty
	// means every caller allowed to land. The identity is set by agent
	// authentication only: an agent name, or "shared" for callers of the
	// shared token. Without [[graft.agent]] callers have no identity, and a
	// non-empty list admits nobody. The user of the request is never used.
	Actors []string `json:"actors" toml:"actors,omitempty"`
}

const (
	// DefaultGraftResolverTimeout is the default GraftResolver.Timeout.
	DefaultGraftResolverTimeout = "120s"
	// DefaultGraftResolverMaxFiles is the default GraftResolver.MaxFiles.
	DefaultGraftResolverMaxFiles = 20
	// DefaultGraftResolverMaxFileBytes is the default GraftResolver.MaxFileBytes.
	DefaultGraftResolverMaxFileBytes = 1 << 20
)

// Validate reports malformed policy rules at load time: an invalid Actor or
// Refs glob, or an empty Refs entry (which would never match, so a deny rule
// written that way would silently never fire).
func (g Graft) Validate() error {
	for i, rule := range g.Policy {
		if rule.Actor != "" && rule.Actor != "*" {
			if _, err := path.Match(rule.Actor, ""); err != nil {
				return fmt.Errorf("graft policy rule %d: actor %q: %w", i+1, rule.Actor, err)
			}
		}

		for _, glob := range rule.Refs {
			if glob == "" {
				return fmt.Errorf("graft policy rule %d: empty refs entry", i+1)
			}

			// The "**" suffix ("/**" or a bare "**") is handled outside path.Match.
			pattern := strings.TrimSuffix(glob, "**")
			if _, err := path.Match(pattern, ""); err != nil {
				return fmt.Errorf("graft policy rule %d: ref %q: %w", i+1, glob, err)
			}
		}
	}

	if err := g.Land.Batch.validate(); err != nil {
		return err
	}
	if err := g.Review.validate(); err != nil {
		return err
	}
	if _, err := g.Search.TimeoutOrDefault(); err != nil {
		return err
	}
	if g.Derived.Workers < 0 || g.Derived.QueueSize < 0 {
		return fmt.Errorf("graft derived: workers and queue_size must not be negative")
	}
	if err := g.Storage.validate(g.Standalone); err != nil {
		return err
	}

	return g.Land.Resolver.validate()
}

func (r GraftResolver) validate() error {
	if !r.Enabled {
		return nil
	}
	if (len(r.Command) == 0) == (r.URL == "") {
		return fmt.Errorf("graft land resolver: exactly one of command and url must be set")
	}
	if r.URL != "" && !strings.HasPrefix(r.URL, "http://") && !strings.HasPrefix(r.URL, "https://") {
		return fmt.Errorf("graft land resolver: url %q must start with http:// or https://", r.URL)
	}
	if _, err := r.TimeoutOrDefault(); err != nil {
		return err
	}
	if r.MaxFiles < 0 || r.MaxFileBytes < 0 {
		return fmt.Errorf("graft land resolver: max_files and max_file_bytes must not be negative")
	}
	for _, list := range [][]string{r.PathsAllow, r.PathsDeny, r.Actors} {
		for _, glob := range list {
			if glob == "" {
				return fmt.Errorf("graft land resolver: empty glob")
			}
			if _, err := path.Match(strings.TrimSuffix(glob, "**"), ""); err != nil {
				return fmt.Errorf("graft land resolver: glob %q: %w", glob, err)
			}
		}
	}
	return nil
}

// TimeoutOrDefault parses Timeout, applying the default.
func (r GraftResolver) TimeoutOrDefault() (time.Duration, error) {
	s := r.Timeout
	if s == "" {
		s = DefaultGraftResolverTimeout
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("graft land resolver: invalid timeout %q", r.Timeout)
	}
	return d, nil
}

// MaxFilesOrDefault returns MaxFiles or its default.
func (r GraftResolver) MaxFilesOrDefault() int {
	if r.MaxFiles > 0 {
		return r.MaxFiles
	}
	return DefaultGraftResolverMaxFiles
}

// MaxFileBytesOrDefault returns MaxFileBytes or its default.
func (r GraftResolver) MaxFileBytesOrDefault() int64 {
	if r.MaxFileBytes > 0 {
		return r.MaxFileBytes
	}
	return DefaultGraftResolverMaxFileBytes
}

// GraftPolicyRule is one access rule for standalone mode.
type GraftPolicyRule struct {
	// Actor is a glob (path.Match syntax) matched against the request's
	// identity; "*" or empty matches everybody.
	Actor string `json:"actor" toml:"actor,omitempty"`
	// Refs are globs (path.Match syntax, plus a trailing "/**" meaning "any
	// depth" and any other trailing "**" matching any remainder, including
	// "/") matched against fully qualified reference names such as
	// "refs/heads/main". Empty matches every reference; an empty entry is
	// rejected at load time.
	Refs []string `json:"refs" toml:"refs,omitempty"`
	// Allow decides whether matching updates are allowed or denied.
	Allow bool `json:"allow" toml:"allow,omitempty"`
}

// GraftAgent is one agent credential.
type GraftAgent struct {
	// Name identifies the agent; it is used as the request identity, as the
	// actor of changesets and as the user ID passed to hooks.
	Name string `json:"name" toml:"name,omitempty"`
	// Token is the agent's secret. Either Token or TokenFile must be set.
	Token string `json:"token" toml:"token,omitempty"`
	// TokenFile is a file containing the agent's secret.
	TokenFile string `json:"token_file" toml:"token_file,omitempty"`
}

// GraftObjectService configures the ObjectService.
type GraftObjectService struct {
	// MaxObjects bounds the number of object IDs per GetObjects request.
	// Zero means DefaultGraftMaxObjects.
	MaxObjects int `json:"max_objects" toml:"max_objects,omitempty"`
	// ChunkSize is the maximum number of data bytes per GetObjects message.
	// Zero means DefaultGraftChunkSize.
	ChunkSize int `json:"chunk_size" toml:"chunk_size,omitempty"`
}

// GraftChangelog configures the changeset log.
type GraftChangelog struct {
	// Disabled turns off recording of reference updates.
	Disabled bool `json:"disabled" toml:"disabled,omitempty"`
}

// GraftDerived configures derived-data caching.
type GraftDerived struct {
	// Disabled turns off the derived-data cache: queries are computed
	// from scratch on every call, as in upstream Gitaly.
	Disabled bool `json:"disabled" toml:"disabled,omitempty"`
	// Eager computes derived data for new commits right after they land,
	// instead of lazily on first read.
	Eager bool `json:"eager" toml:"eager,omitempty"`
	// CommitGraph writes the commit-graph incrementally after committed
	// reference updates (debounced per repository), in addition to
	// housekeeping. Off by default so that a server without Graft
	// configuration behaves like upstream Gitaly.
	CommitGraph bool `json:"commit_graph" toml:"commit_graph,omitempty"`
	// Workers is the number of background workers computing derived data
	// eagerly. Zero means DefaultGraftDerivedWorkers.
	Workers int `json:"workers" toml:"workers,omitempty"`
	// QueueSize bounds the eager computation queue; commits beyond it are
	// dropped (and computed lazily on first read). Zero means
	// DefaultGraftDerivedQueueSize.
	QueueSize int `json:"queue_size" toml:"queue_size,omitempty"`
}

const (
	// DefaultGraftDerivedWorkers is the default GraftDerived.Workers.
	DefaultGraftDerivedWorkers = 2
	// DefaultGraftDerivedQueueSize is the default GraftDerived.QueueSize.
	DefaultGraftDerivedQueueSize = 10000
)

// WorkersOrDefault returns Workers or its default.
func (d GraftDerived) WorkersOrDefault() int {
	if d.Workers > 0 {
		return d.Workers
	}
	return DefaultGraftDerivedWorkers
}

// QueueSizeOrDefault returns QueueSize or its default.
func (d GraftDerived) QueueSizeOrDefault() int {
	if d.QueueSize > 0 {
		return d.QueueSize
	}
	return DefaultGraftDerivedQueueSize
}

const (
	// DefaultGraftMaxObjects is the default ObjectService.MaxObjects.
	DefaultGraftMaxObjects = 1000
	// DefaultGraftChunkSize is the default ObjectService.ChunkSize.
	DefaultGraftChunkSize = 1 << 20
)

// MaxObjectsOrDefault returns MaxObjects or its default.
func (c GraftObjectService) MaxObjectsOrDefault() int {
	if c.MaxObjects > 0 {
		return c.MaxObjects
	}
	return DefaultGraftMaxObjects
}

// ChunkSizeOrDefault returns ChunkSize or its default.
func (c GraftObjectService) ChunkSizeOrDefault() int {
	if c.ChunkSize > 0 {
		return c.ChunkSize
	}
	return DefaultGraftChunkSize
}
