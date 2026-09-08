package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"

	"github.com/devidevio/gitone/internal/git"
)

const (
	// PublicFile is the committed configuration, LocalFile the ignored one.
	PublicFile = ".gitone.yml"
	LocalFile  = ".gitone.local.yml"
	// ProjectIgnoreFile is the ordinary owned ignore file setup creates.
	ProjectIgnoreFile = ".gitignore"

	// OriginRemote is the only remote GitOne fetches from or publishes to.
	OriginRemote = "origin"

	// Public and Private are the configured visibilities. They grant and deny
	// nothing by themselves; they state what a repository is published as, so
	// a change that moves ownership between them can be reviewed as one.
	Public  = "public"
	Private = "private"

	// refRules describes native Git's ref-name grammar in the one sentence
	// every message about a rejected identifier uses.
	refRules = "no space, no ~ ^ : ? * [ \\, no .. or @{, no leading, trailing or doubled /, no leading . or - and no trailing . or .lock"

	// ProtectedReason is why a protected path can never enter a repository.
	// Ownership checks and worktree scanning report the same sentence.
	ProtectedReason = "path is protected and must never be committed"

	// PushAllowed is the default publication policy. PushDisabled keeps a
	// repository local even when it has outgoing commits.
	PushAllowed  = "allowed"
	PushDisabled = "disabled"

	internalDirectory = ".gitone"
)

// ReservedPaths are the GitOne paths themselves. They can never be owned,
// staged or committed by a managed repository and are usable as a Git
// pathspec that selects exactly them.
var ReservedPaths = []string{internalDirectory, LocalFile}

var repositoryName = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
var literalPatternCharacters = strings.NewReplacer("?", `\?`, "[", `\[`, "]", `\]`)

type Error struct {
	Code    string
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e.Err == nil {
		return e.Code + " " + e.Message
	}
	return e.Code + " " + e.Message + ": " + e.Err.Error()
}

func (e *Error) Unwrap() error {
	return e.Err
}

type Config struct {
	Version        int
	DefaultBranch  string
	ProtectedPaths []string
	Push           PushRules
	Repositories   map[string]Repository
}

type PushRules struct {
	RequireCleanWorktree bool
}

type Repository struct {
	Visibility string
	Push       string
	Paths      []string
	Remotes    map[string]string
}

func (c *Config) RepositoryNames() []string {
	return slices.Sorted(maps.Keys(c.Repositories))
}

func (r Repository) RemoteNames() []string {
	return slices.Sorted(maps.Keys(r.Remotes))
}

// Matcher resolves project-relative paths to the repositories owning them.
type Matcher struct {
	patterns  []ownedPattern
	protected []pattern
}

// Matcher builds the path matcher of a configuration. It fails closed: an
// unusable pattern is an error instead of a silently dropped ownership claim.
func (c *Config) Matcher() (*Matcher, error) {
	patterns, err := c.patterns()
	if err != nil {
		return nil, err
	}
	protected, err := c.protectedPatterns()
	if err != nil {
		return nil, err
	}
	return &Matcher{patterns: patterns, protected: protected}, nil
}

func (c *Config) patterns() ([]ownedPattern, error) {
	var patterns []ownedPattern
	for _, name := range c.RepositoryNames() {
		for _, configuredPath := range c.Repositories[name].Paths {
			parsed, err := parsePattern(configuredPath)
			if err != nil {
				return nil, invalid("repository %q path %q is invalid: %v", name, configuredPath, err)
			}
			patterns = append(patterns, ownedPattern{repository: name, pattern: parsed})
		}
	}
	return patterns, nil
}

func (c *Config) protectedPatterns() ([]pattern, error) {
	patterns := make([]pattern, 0, len(c.ProtectedPaths))
	for _, configuredPath := range c.ProtectedPaths {
		parsed, err := parsePattern(configuredPath)
		if err != nil {
			return nil, invalid("protected path %q is invalid: %v", configuredPath, err)
		}
		for _, required := range []string{PublicFile, ProjectIgnoreFile} {
			if parsed.matches(required, true) {
				return nil, invalid("protected path %q matches %s, which must be owned", configuredPath, required)
			}
		}
		patterns = append(patterns, parsed)
	}
	return patterns, nil
}

// Owners returns the repositories owning relativePath and the repositories
// that would own it if case were ignored. Both results are sorted by name.
func (m *Matcher) Owners(relativePath string) (exact, fold []string) {
	for _, owned := range m.patterns {
		if owned.matches(relativePath, false) {
			exact = appendUnique(exact, owned.repository)
		}
		if owned.matches(relativePath, true) {
			fold = appendUnique(fold, owned.repository)
		}
	}
	return exact, fold
}

// Protected reports whether a path matches a configured hard-deny rule. The
// case-insensitive check keeps the rule stable across Linux and macOS.
func (m *Matcher) Protected(relativePath string) bool {
	return slices.ContainsFunc(m.protected, func(pattern pattern) bool {
		return pattern.matches(relativePath, true)
	})
}

func appendUnique(names []string, name string) []string {
	if slices.Contains(names, name) {
		return names
	}
	return append(names, name)
}

type rawConfig struct {
	Version       int                       `yaml:"version"`
	DefaultBranch string                    `yaml:"default_branch"`
	Rules         rawRules                  `yaml:"rules"`
	Repositories  map[string]*rawRepository `yaml:"repositories"`
}

type rawRules struct {
	ProtectedPaths []string     `yaml:"protected_paths"`
	Push           rawPushRules `yaml:"push"`
}

type rawPushRules struct {
	RequireCleanWorktree *bool `yaml:"require_clean_worktree"`
}

type rawRepository struct {
	Remote     *string            `yaml:"remote"`
	Visibility string             `yaml:"visibility"`
	Push       string             `yaml:"push"`
	Paths      []string           `yaml:"paths"`
	Remotes    map[string]*string `yaml:"remotes"`
}

func Load(start string) (*Config, string, error) {
	root, err := DiscoverRoot(start)
	if err != nil {
		return nil, "", err
	}
	committed, err := os.ReadFile(filepath.Join(root, PublicFile))
	if err != nil {
		return nil, "", configError("CONFIG001", "configuration missing", err)
	}
	configuration, err := Effective(committed, root)
	if err != nil {
		return nil, "", err
	}
	return configuration, root, nil
}

// Effective merges committed configuration bytes into the local file of root
// and validates the result. Load applies it to the committed file of the
// project; pull applies it to an incoming one, which stays untrusted policy
// input until it validates against the unchanged local file.
func Effective(committed []byte, root string) (*Config, error) {
	local, err := os.ReadFile(filepath.Join(root, LocalFile))
	switch {
	case errors.Is(err, os.ErrNotExist):
		local = nil
	case err != nil:
		return nil, configError("CONFIG001", "configuration missing", err)
	}
	return Projected(committed, local)
}

// Projected merges two configuration files that exist only as bytes, which is
// what a command validates before it writes either of them. A nil local
// stands for a project without the ignored file.
func Projected(committed, local []byte) (*Config, error) {
	merged, err := decodeBytes(PublicFile, committed)
	if err != nil {
		return nil, err
	}
	if err := normalizeRemotes(merged); err != nil {
		return nil, err
	}
	if local == nil {
		return validate(merged)
	}
	overriding, err := decodeBytes(LocalFile, local)
	if err != nil {
		return nil, err
	}
	if err := normalizeRemotes(overriding); err != nil {
		return nil, err
	}
	merge(merged, overriding)
	return validate(merged)
}

func DiscoverRoot(start string) (string, error) {
	current, err := filepath.Abs(start)
	if err != nil {
		return "", configError("CONFIG001", "configuration missing", err)
	}
	info, err := os.Stat(current)
	if err != nil {
		return "", configError("CONFIG001", "configuration missing", err)
	}
	if !info.IsDir() {
		return "", configError("CONFIG001", "configuration missing", fmt.Errorf("%s is not a directory", current))
	}

	for {
		_, err := os.Stat(filepath.Join(current, PublicFile))
		if err == nil {
			return current, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", configError("CONFIG001", "configuration missing", err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			// Reaching the filesystem root without a configuration means this
			// is not a GitOne project yet, which setup is what creates.
			return "", configError("CONFIG001", "configuration missing: run gitone setup to create "+PublicFile, nil)
		}
		current = parent
	}
}

func decodeOptionalFile(name string) (*rawConfig, error) {
	configuration, err := decodeFile(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return configuration, err
}

func decodeFile(name string) (*rawConfig, error) {
	contents, err := os.ReadFile(name)
	if err != nil {
		return nil, configError("CONFIG001", "configuration missing", err)
	}
	return decodeBytes(filepath.Base(name), contents)
}

func decodeBytes(name string, contents []byte) (*rawConfig, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	decoder.KnownFields(true)
	configuration := new(rawConfig)
	if err := decoder.Decode(configuration); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, invalid("%s is empty", name)
		}
		return nil, configError("CONFIG002", "invalid YAML", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			err = errors.New("multiple YAML documents are not supported")
		}
		return nil, configError("CONFIG002", "invalid YAML", err)
	}
	return configuration, nil
}

func normalizeRemotes(configuration *rawConfig) error {
	for _, name := range slices.Sorted(maps.Keys(configuration.Repositories)) {
		repository := configuration.Repositories[name]
		if repository == nil || repository.Remote == nil {
			continue
		}
		if repository.Remotes == nil {
			repository.Remotes = make(map[string]*string)
		}
		if origin := repository.Remotes[OriginRemote]; origin != nil && *origin != *repository.Remote {
			return invalid("repository %q has conflicting origin definitions", name)
		}
		repository.Remotes[OriginRemote] = repository.Remote
		repository.Remote = nil
	}
	return nil
}

func merge(target, override *rawConfig) {
	if override.Version != 0 {
		target.Version = override.Version
	}
	if target.Repositories == nil {
		target.Repositories = make(map[string]*rawRepository)
	}
	if override.DefaultBranch != "" {
		target.DefaultBranch = override.DefaultBranch
	}
	for _, protected := range override.Rules.ProtectedPaths {
		if !slices.Contains(target.Rules.ProtectedPaths, protected) {
			target.Rules.ProtectedPaths = append(target.Rules.ProtectedPaths, protected)
		}
	}
	// The push rules are the only merge that is not a plain override: the
	// ignored file is never reviewed, so it may tighten a committed policy
	// but never weaken it.
	if override.Rules.Push.RequireCleanWorktree != nil && *override.Rules.Push.RequireCleanWorktree {
		target.Rules.Push.RequireCleanWorktree = override.Rules.Push.RequireCleanWorktree
	}
	for name, localRepository := range override.Repositories {
		if localRepository == nil {
			continue
		}
		publicRepository := target.Repositories[name]
		if publicRepository == nil {
			publicRepository = &rawRepository{}
			target.Repositories[name] = publicRepository
		}
		if localRepository.Visibility != "" {
			publicRepository.Visibility = localRepository.Visibility
		}
		if localRepository.Push == PushDisabled {
			publicRepository.Push = localRepository.Push
		}
		if localRepository.Paths != nil {
			publicRepository.Paths = localRepository.Paths
		}
		for _, remoteName := range slices.Sorted(maps.Keys(localRepository.Remotes)) {
			remoteURL := localRepository.Remotes[remoteName]
			if remoteURL == nil {
				continue
			}
			if publicRepository.Remotes == nil {
				publicRepository.Remotes = make(map[string]*string)
			}
			publicRepository.Remotes[remoteName] = remoteURL
		}
	}
}

func validate(raw *rawConfig) (*Config, error) {
	if raw.Version != 1 {
		return nil, invalid("version must be 1")
	}
	if len(raw.Repositories) == 0 {
		return nil, invalid("at least one repository is required")
	}
	if err := ValidateBranch(raw.DefaultBranch); err != nil {
		return nil, unusable(err, "default_branch must be a valid branch: %v", err)
	}

	configuration := &Config{
		Version:        raw.Version,
		DefaultBranch:  raw.DefaultBranch,
		ProtectedPaths: slices.Clone(raw.Rules.ProtectedPaths),
		Repositories:   make(map[string]Repository, len(raw.Repositories)),
	}
	if raw.Rules.Push.RequireCleanWorktree != nil {
		configuration.Push.RequireCleanWorktree = *raw.Rules.Push.RequireCleanWorktree
	}
	if _, err := configuration.protectedPatterns(); err != nil {
		return nil, err
	}

	for _, name := range slices.Sorted(maps.Keys(raw.Repositories)) {
		repository := raw.Repositories[name]
		if err := ValidateName(name); err != nil {
			return nil, invalid("repository name %q is invalid", name)
		}
		if repository == nil {
			return nil, invalid("repository %q is empty", name)
		}
		if err := ValidateVisibility(repository.Visibility); err != nil {
			return nil, invalid("repository %q must have public or private visibility", name)
		}
		push := repository.Push
		if push == "" {
			push = PushAllowed
		}
		if err := ValidatePush(push); err != nil {
			return nil, invalid("repository %q push must be allowed or disabled", name)
		}
		if len(repository.Paths) == 0 {
			return nil, invalid("repository %q must have at least one path", name)
		}

		validated := Repository{
			Visibility: repository.Visibility,
			Push:       push,
			Paths:      slices.Clone(repository.Paths),
			Remotes:    make(map[string]string, len(repository.Remotes)),
		}
		for _, remoteName := range slices.Sorted(maps.Keys(repository.Remotes)) {
			remoteURL := repository.Remotes[remoteName]
			if remoteURL == nil {
				return nil, invalid("repository %q has an invalid remote", name)
			}
			if err := ValidateRemote(remoteName, *remoteURL); err != nil {
				return nil, unusable(err, "repository %q has an invalid remote: %v", name, err)
			}
			validated.Remotes[remoteName] = *remoteURL
		}
		configuration.Repositories[name] = validated
	}
	patterns, err := configuration.patterns()
	if err != nil {
		return nil, err
	}
	if err := validatePatternDuplicates(patterns); err != nil {
		return nil, err
	}
	return configuration, nil
}

type ownedPattern struct {
	repository string
	pattern
}

// pattern is a validated ownership pattern: a sequence of path segments in
// which * matches inside one segment and a single ** segment matches zero or
// more complete segments. The segments before and after ** are kept apart so
// matching never has to search for a split point.
type pattern struct {
	original  string
	prefix    []string
	suffix    []string
	recursive bool
}

// ValidateName, ValidateVisibility, ValidateBranch, ValidatePattern and
// ValidateRemote define the field rules of one repository once. The loader
// applies them to a decoded file; setup applies them to a single answer so an
// invalid value can be asked again.

func ValidateName(name string) error {
	if !repositoryName.MatchString(name) {
		return errors.New("repository names start with a letter and contain only a-z, 0-9, - and _")
	}
	if name == "all" {
		return errors.New("repository name \"all\" is reserved")
	}
	return nil
}

func ValidateVisibility(visibility string) error {
	if visibility != Public && visibility != Private {
		return errors.New("visibility is public or private")
	}
	return nil
}

func ValidatePush(push string) error {
	if push != PushAllowed && push != PushDisabled {
		return errors.New("push is allowed or disabled")
	}
	return nil
}

func ValidateBranch(branch string) error {
	if strings.TrimSpace(branch) == "" {
		return errors.New("branch is empty")
	}
	// Native Git accepts both names as refs, but a leading - is read as an
	// option by every git command GitOne invokes and HEAD is not a branch.
	if strings.HasPrefix(branch, "-") || branch == "HEAD" {
		return errors.New("branch must not start with - or be HEAD")
	}
	return checkRefFormat("refs/heads/"+branch, "branch")
}

// checkRefFormat applies native Git's own ref grammar to the ref subject
// becomes, so a configured identifier is accepted here exactly when the Git
// commands GitOne later invokes accept it. Control characters are not checked
// separately: check-ref-format rejects them itself.
func checkRefFormat(reference, subject string) error {
	valid, err := git.CheckRefFormat(reference)
	switch {
	case err != nil:
		return err
	case !valid:
		return errors.New(subject + " is not a valid Git name: " + refRules)
	}
	return nil
}

// ValidatePattern reports why configured is not a usable ownership pattern.
func ValidatePattern(configured string) error {
	_, err := parsePattern(configured)
	return err
}

func ValidateRemote(name, url string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("remote name is empty")
	}
	if strings.HasPrefix(name, "-") {
		return errors.New("remote name must not start with -")
	}
	// git remote add validates a name as the remote-tracking ref it builds
	// from it, so the same ref is checked here.
	if err := checkRefFormat("refs/remotes/"+name+"/HEAD", "remote name"); err != nil {
		return err
	}
	if strings.TrimSpace(url) == "" || hasASCIIControl(url) {
		return errors.New("remote URL is empty or contains unsupported characters")
	}
	return nil
}

// ValidatePath reports why relativePath is not a usable project-relative path.
// It defines the grammar of concrete paths - discovered files and command-line
// arguments; ValidatePattern defines the grammar of configured patterns.
func ValidatePath(relativePath string) error {
	if relativePath == "" {
		return errors.New("path is empty")
	}
	if filepath.IsAbs(relativePath) || strings.HasPrefix(relativePath, "/") {
		return errors.New("path must be relative")
	}
	if strings.Contains(relativePath, `\`) || hasASCIIControl(relativePath) {
		return errors.New("path contains unsupported characters")
	}
	if path.Clean(relativePath) != relativePath {
		return errors.New("path is not normalized")
	}
	for _, segment := range strings.Split(relativePath, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return errors.New("path contains an invalid segment")
		}
	}
	return nil
}

// IsReserved reports whether relativePath belongs to GitOne itself and can
// therefore never be owned, staged or committed by a managed repository.
func IsReserved(relativePath string) bool {
	return relativePath == LocalFile || relativePath == internalDirectory || strings.HasPrefix(relativePath, internalDirectory+"/")
}

// parsePattern validates a configured ownership pattern. The grammar is
// deliberately small: exact segments, * inside a segment and at most one **
// segment. Everything Git pathspecs additionally allow stays out, because a
// pattern nobody can read is a pattern nobody can audit.
func parsePattern(configured string) (pattern, error) {
	if configured == "" {
		return pattern{}, errors.New("path is empty")
	}
	if filepath.IsAbs(configured) || strings.HasPrefix(configured, "/") {
		return pattern{}, errors.New("path must be relative")
	}
	if strings.Contains(configured, `\`) || hasASCIIControl(configured) {
		return pattern{}, errors.New("path contains unsupported characters")
	}
	parsed := pattern{original: configured, prefix: strings.Split(configured, "/")}
	for index, segment := range parsed.prefix {
		switch {
		case segment == "" || segment == "." || segment == "..":
			return pattern{}, errors.New("path contains an invalid segment")
		case segment == "**":
			if parsed.recursive {
				return pattern{}, errors.New("** may appear only once")
			}
			parsed.recursive = true
			parsed.prefix, parsed.suffix = parsed.prefix[:index], parsed.prefix[index+1:]
		case strings.Contains(segment, "**"):
			return pattern{}, errors.New("** must occupy a complete path segment")
		}
	}
	// Only the leading wildcard-free segments name a concrete location, so
	// they are what can name a reserved one. A wildcard reaching a reserved
	// path is refused where every command already checks it.
	if IsReserved(strings.Join(literalPrefix(parsed.prefix), "/")) {
		return pattern{}, errors.New("path is reserved")
	}
	return parsed, nil
}

// literalPrefix returns the leading segments that contain no wildcard.
func literalPrefix(segments []string) []string {
	for index, segment := range segments {
		if strings.Contains(segment, "*") {
			return segments[:index]
		}
	}
	return segments
}

// matches reports whether relativePath is owned by the pattern. Matching is
// case-sensitive; the folded form only detects case-related ambiguity.
func (p pattern) matches(relativePath string, foldCase bool) bool {
	segments := strings.Split(relativePath, "/")
	if !p.recursive {
		return matchSegments(p.prefix, segments, foldCase)
	}
	if len(segments) < len(p.prefix)+len(p.suffix) {
		return false
	}
	// A trailing ** owns everything inside its prefix, not the prefix itself,
	// which keeps src/** meaning what it always meant.
	if len(p.suffix) == 0 && len(segments) == len(p.prefix) {
		return false
	}
	return matchSegments(p.prefix, segments[:len(p.prefix)], foldCase) &&
		matchSegments(p.suffix, segments[len(segments)-len(p.suffix):], foldCase)
}

func matchSegments(patterns, segments []string, foldCase bool) bool {
	if len(patterns) != len(segments) {
		return false
	}
	for index, segment := range segments {
		expected := patterns[index]
		if foldCase {
			expected, segment = FoldPath(expected), FoldPath(segment)
		}
		// Only * is a wildcard in GitOne's grammar. Quote path.Match's other
		// metacharacters so valid concrete file names stay literal.
		expected = literalPatternCharacters.Replace(expected)
		if ok, _ := path.Match(expected, segment); !ok {
			return false
		}
	}
	return true
}

// Owns reports whether the configured ownership pattern matches relativePath.
// An invalid pattern owns nothing; validation reports it separately.
func Owns(configured, relativePath string) bool {
	parsed, err := parsePattern(configured)
	return err == nil && parsed.matches(relativePath, false)
}

// validatePatternDuplicates rejects the same pattern twice, inside one
// repository and across repositories, including patterns that differ only by
// case. Patterns that merely overlap are allowed: a concrete path matching two
// repositories is caught where the path is known, not guessed here.
func validatePatternDuplicates(patterns []ownedPattern) error {
	for i := range patterns {
		for j := range i {
			if FoldPath(patterns[i].original) != FoldPath(patterns[j].original) {
				continue
			}
			suffix := ""
			if patterns[i].original != patterns[j].original {
				suffix = " case-insensitively"
			}
			return invalid("path %q in repository %q duplicates %q in repository %q%s", patterns[i].original, patterns[i].repository, patterns[j].original, patterns[j].repository, suffix)
		}
	}
	return nil
}

// FoldPath returns a stable key for case-insensitive path comparisons.
func FoldPath(relativePath string) string {
	return strings.Map(func(character rune) rune {
		smallest := character
		for folded := unicode.SimpleFold(character); folded != character; folded = unicode.SimpleFold(folded) {
			if folded < smallest {
				smallest = folded
			}
		}
		return smallest
	}, relativePath)
}

func hasASCIIControl(value string) bool {
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}

func invalid(format string, arguments ...any) error {
	return configError("CONFIG003", "invalid configuration", fmt.Errorf(format, arguments...))
}

// unusable reports a rejected field as invalid configuration, except when
// git itself could not run: a broken Git installation is an operational
// failure and must never be reported as a bad configured value.
func unusable(err error, format string, arguments ...any) error {
	if errors.Is(err, git.ErrUnavailable) {
		return err
	}
	return invalid(format, arguments...)
}

func configError(code, message string, err error) error {
	return &Error{Code: code, Message: message, Err: err}
}

// OwnerIssue classifies why a path has no single owner.
type OwnerIssue int

const (
	OwnerValid OwnerIssue = iota
	OwnerUnsafe
	OwnerUnassigned
	OwnerAmbiguous
)

// Owner returns the single repository that may manage relativePath, or the
// classified reason no repository may. Every command that decides whether a
// path may enter or leave a repository uses this one rule.
func (m *Matcher) Owner(relativePath string) (string, OwnerIssue, string) {
	if IsReserved(relativePath) {
		return "", OwnerUnsafe, "reserved GitOne paths must never be committed"
	}
	if err := ValidatePath(relativePath); err != nil {
		return "", OwnerUnsafe, err.Error()
	}
	if m.Protected(relativePath) {
		return "", OwnerUnsafe, ProtectedReason
	}
	owners, fold := m.Owners(relativePath)
	switch {
	case len(fold) != len(owners):
		return "", OwnerUnsafe, "path matches " + strings.Join(fold, ", ") + " only when case is ignored"
	case len(owners) == 0:
		return "", OwnerUnassigned, "path is not assigned to any repository"
	case len(owners) > 1:
		return "", OwnerAmbiguous, "path matches " + strings.Join(owners, ", ")
	}
	return owners[0], OwnerValid, ""
}

// Unowned reports why repository name must not manage relativePath, or the
// empty string when it owns the path exactly.
func (m *Matcher) Unowned(name, relativePath string) string {
	_, reason := m.UnownedIssue(name, relativePath)
	return reason
}

// UnownedIssue is Unowned with the classification kept, so a command can tell
// an unassigned path from every other ownership failure without reading the
// reason text. A path owned by another repository is OwnerAmbiguous: it has an
// owner, but not this one.
func (m *Matcher) UnownedIssue(name, relativePath string) (OwnerIssue, string) {
	owner, issue, reason := m.Owner(relativePath)
	switch {
	case reason != "":
		return issue, reason
	case owner != name:
		return OwnerAmbiguous, "path is owned by " + owner
	}
	return OwnerValid, ""
}

// DefinedNames is every repository name one configuration file defines by
// itself, before any merge. It is how a command tells a repository an
// incoming .gitone.yml adds from one .gitone.local.yml has always defined:
// the merged configuration names both the same way, and only the file a name
// comes from decides whether accepting it would attach a remote-controlled
// remote to a privately defined repository.
func DefinedNames(name string, contents []byte) ([]string, error) {
	raw, err := decodeBytes(name, contents)
	if err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Keys(raw.Repositories)), nil
}
