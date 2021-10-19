package lang

import (
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/princjef/gomarkdoc/logger"
)

type (
	// Config defines contextual information used to resolve documentation for
	// a construct.
	Config struct {
		FileSet *token.FileSet
		Level   int
		Repo    *Repo
		PkgDir  string
		WorkDir string
		Log     logger.Logger
	}

	// Repo represents information about a repository relevant to documentation
	// generation.
	Repo struct {
		Remote        string
		DefaultBranch string
		PathFromRoot  string
	}

	// Location holds information for identifying a position within a file and
	// repository, if present.
	Location struct {
		Start    Position
		End      Position
		Filepath string
		WorkDir  string
		Repo     *Repo
	}

	// Position represents a line and column number within a file.
	Position struct {
		Line int
		Col  int
	}

	// ConfigOption modifies the Config generated by NewConfig.
	ConfigOption func(c *Config) error
)

// NewConfig generates a Config for the provided package directory. It will
// resolve the filepath and attempt to determine the repository containing the
// directory. If no repository is found, the Repo field will be set to nil. An
// error is returned if the provided directory is invalid.
func NewConfig(log logger.Logger, workDir string, pkgDir string, opts ...ConfigOption) (*Config, error) {
	cfg := &Config{
		FileSet: token.NewFileSet(),
		Level:   1,
		Log:     log,
	}

	for _, opt := range opts {
		if err := opt(cfg); err != nil {
			return nil, err
		}
	}

	var err error

	cfg.PkgDir, err = filepath.Abs(pkgDir)
	if err != nil {
		return nil, err
	}

	cfg.WorkDir, err = filepath.Abs(workDir)
	if err != nil {
		return nil, err
	}

	if cfg.Repo == nil || cfg.Repo.Remote == "" || cfg.Repo.DefaultBranch == "" || cfg.Repo.PathFromRoot == "" {
		repo, err := getRepoForDir(log, cfg.WorkDir, cfg.PkgDir, cfg.Repo)
		if err != nil {
			log.Infof("unable to resolve repository due to error: %s", err)
			cfg.Repo = nil
			return cfg, nil
		}

		log.Debugf(
			"resolved repository with remote %s, default branch %s, path from root %s",
			repo.Remote,
			repo.DefaultBranch,
			repo.PathFromRoot,
		)
		cfg.Repo = repo
	} else {
		log.Debugf("skipping repository resolution because all values have manual overrides")
	}

	return cfg, nil
}

// Inc copies the Config and increments the level by the provided step.
func (c *Config) Inc(step int) *Config {
	return &Config{
		FileSet: c.FileSet,
		Level:   c.Level + step,
		PkgDir:  c.PkgDir,
		WorkDir: c.WorkDir,
		Repo:    c.Repo,
		Log:     c.Log,
	}
}

// ConfigWithRepoOverrides defines a set of manual overrides for the repository
// information to be used in place of automatic repository detection.
func ConfigWithRepoOverrides(overrides *Repo) ConfigOption {
	return func(c *Config) error {
		if overrides == nil {
			return nil
		}

		if overrides.PathFromRoot != "" {
			// Convert it to the right pathing system
			unslashed := filepath.FromSlash(overrides.PathFromRoot)

			if len(unslashed) == 0 || unslashed[0] != filepath.Separator {
				return fmt.Errorf("provided repository path %s must be absolute", overrides.PathFromRoot)
			}

			overrides.PathFromRoot = unslashed
		}

		c.Repo = overrides
		return nil
	}
}

func getRepoForDir(log logger.Logger, wd string, dir string, ri *Repo) (*Repo, error) {
	if ri == nil {
		ri = &Repo{}
	}

	repo, err := git.PlainOpenWithOptions(dir, &git.PlainOpenOptions{
		DetectDotGit: true,
	})
	if err != nil {
		return nil, err
	}

	// Set the path from root if there wasn't one
	if ri.PathFromRoot == "" {
		t, err := repo.Worktree()
		if err != nil {
			return nil, err
		}

		// Get the path from the root of the repo to the working dir, then make
		// it absolute (i.e. prefix with /).
		p, err := filepath.Rel(t.Filesystem.Root(), wd)
		if err != nil {
			return nil, err
		}

		ri.PathFromRoot = filepath.Join(string(filepath.Separator), p)
	}

	// No need to check remotes if we already have a url and a default branch
	if ri.Remote != "" && ri.DefaultBranch != "" {
		return ri, nil
	}

	remotes, err := repo.Remotes()
	if err != nil {
		return nil, err
	}

	for _, r := range remotes {
		if repo, ok := processRemote(log, repo, r, *ri); ok {
			ri = repo
			break
		}
	}

	// If there's no "origin", just use the first remote
	if ri.DefaultBranch == "" || ri.Remote == "" {
		if len(remotes) == 0 {
			return nil, errors.New("no remotes found for repository")
		}

		repo, ok := processRemote(log, repo, remotes[0], *ri)
		if !ok {
			return nil, errors.New("no remotes found for repository")
		}

		ri = repo
	}

	return ri, nil
}

func processRemote(log logger.Logger, repository *git.Repository, remote *git.Remote, ri Repo) (*Repo, bool) {
	repo := &ri

	c := remote.Config()

	// TODO: configurable remote name?
	if c.Name != "origin" || len(c.URLs) == 0 {
		log.Debugf("skipping remote because it is not the origin or it has no URLs")
		return nil, false
	}

	// Only detect the default branch if we don't already have one
	if repo.DefaultBranch == "" {
		refs, err := repository.References()
		if err != nil {
			log.Debugf("skipping remote %s because listing its refs failed: %s", c.URLs[0], err)
			return nil, false
		}

		prefix := fmt.Sprintf("refs/remotes/%s/", c.Name)
		headRef := fmt.Sprintf("refs/remotes/%s/HEAD", c.Name)

		for {
			ref, err := refs.Next()
			if err != nil {
				if err == io.EOF {
					break
				}

				log.Debugf("skipping remote %s because listing its refs failed: %s", c.URLs[0], err)
				return nil, false
			}
			defer refs.Close()

			if ref == nil {
				break
			}

			if string(ref.Name()) == headRef && strings.HasPrefix(string(ref.Target()), prefix) {
				repo.DefaultBranch = strings.TrimPrefix(string(ref.Target()), prefix)
				log.Debugf("found default branch %s for remote %s", repo.DefaultBranch, c.URLs[0])
				break
			}
		}

		if repo.DefaultBranch == "" {
			log.Debugf("skipping remote %s because no default branch was found", c.URLs[0])
			return nil, false
		}
	}

	// If we already have the remote from an override, we don't need to detect.
	if repo.Remote != "" {
		return repo, true
	}

	normalized, ok := normalizeRemote(c.URLs[0])
	if !ok {
		log.Debugf("skipping remote %s because its remote URL could not be normalized", c.URLs[0])
		return nil, false
	}

	repo.Remote = normalized
	return repo, true
}

var (
	sshRemoteRegex       = regexp.MustCompile(`^[\w-]+@([^:]+):(.+?)(?:\.git)?$`)
	httpsRemoteRegex     = regexp.MustCompile(`^(https?://)(?:[^@/]+@)?([\w-.]+)(/.+?)?(?:\.git)?$`)
	devOpsSSHV3PathRegex = regexp.MustCompile(`^v3/([^/]+)/([^/]+)/([^/]+)$`)
	devOpsHTTPSPathRegex = regexp.MustCompile(`^/([^/]+)/([^/]+)/_git/([^/]+)$`)
)

func normalizeRemote(remote string) (string, bool) {
	if match := sshRemoteRegex.FindStringSubmatch(remote); match != nil {
		switch match[1] {
		case "ssh.dev.azure.com", "vs-ssh.visualstudio.com":
			if pathMatch := devOpsSSHV3PathRegex.FindStringSubmatch(match[2]); pathMatch != nil {
				// DevOps v3
				return fmt.Sprintf(
					"https://dev.azure.com/%s/%s/_git/%s",
					pathMatch[1],
					pathMatch[2],
					pathMatch[3],
				), true
			}

			return "", false
		default:
			// GitHub and friends
			return fmt.Sprintf("https://%s/%s", match[1], match[2]), true
		}
	}

	if match := httpsRemoteRegex.FindStringSubmatch(remote); match != nil {
		switch {
		case match[2] == "dev.azure.com":
			if pathMatch := devOpsHTTPSPathRegex.FindStringSubmatch(match[3]); pathMatch != nil {
				// DevOps
				return fmt.Sprintf(
					"https://dev.azure.com/%s/%s/_git/%s",
					pathMatch[1],
					pathMatch[2],
					pathMatch[3],
				), true
			}

			return "", false
		case strings.HasSuffix(match[2], ".visualstudio.com"):
			if pathMatch := devOpsHTTPSPathRegex.FindStringSubmatch(match[3]); pathMatch != nil {
				// DevOps (old domain)

				// Pull off the beginning of the domain
				org := strings.SplitN(match[2], ".", 2)[0]
				return fmt.Sprintf(
					"https://dev.azure.com/%s/%s/_git/%s",
					org,
					pathMatch[2],
					pathMatch[3],
				), true
			}

			return "", false
		default:
			// GitHub and friends
			return fmt.Sprintf("%s%s%s", match[1], match[2], match[3]), true
		}
	}

	// TODO: error instead?
	return "", false
}

// NewLocation returns a location for the provided Config and ast.Node
// combination. This is typically not called directly, but is made available via
// the Location() methods of various lang constructs.
func NewLocation(cfg *Config, node ast.Node) Location {
	start := cfg.FileSet.Position(node.Pos())
	end := cfg.FileSet.Position(node.End())

	return Location{
		Start:    Position{start.Line, start.Column},
		End:      Position{end.Line, end.Column},
		Filepath: start.Filename,
		WorkDir:  cfg.WorkDir,
		Repo:     cfg.Repo,
	}
}