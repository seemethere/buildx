package bake

import (
	"context"
	"io/ioutil"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/docker/buildx/build"
	"github.com/docker/buildx/util/platformutil"
	"github.com/docker/docker/pkg/urlutil"
	"github.com/moby/buildkit/session/auth/authprovider"
	"github.com/pkg/errors"
)

func ReadTargets(ctx context.Context, files, targets, overrides []string) (map[string]Target, error) {
	var c Config
	for _, f := range files {
		cfg, err := ParseFile(f)
		if err != nil {
			return nil, err
		}
		c = mergeConfig(c, *cfg)
	}
	o, err := c.newOverrides(overrides)
	if err != nil {
		return nil, err
	}
	m := map[string]Target{}
	for _, n := range targets {
		for _, n := range c.ResolveGroup(n) {
			t, err := c.ResolveTarget(n, o)
			if err != nil {
				return nil, err
			}
			if t != nil {
				m[n] = *t
			}
		}
	}
	return m, nil
}

func ParseFile(fn string) (*Config, error) {
	dt, err := ioutil.ReadFile(fn)
	if err != nil {
		return nil, err
	}

	fnl := strings.ToLower(fn)
	if strings.HasSuffix(fnl, ".yml") || strings.HasSuffix(fnl, ".yaml") {
		return ParseCompose(dt)
	}

	if strings.HasSuffix(fnl, ".json") || strings.HasSuffix(fnl, ".hcl") {
		return ParseHCL(dt)
	}

	cfg, err := ParseCompose(dt)
	if err != nil {
		cfg, err2 := ParseHCL(dt)
		if err2 != nil {
			return nil, errors.Errorf("failed to parse %s: parsing yaml: %s, parsing hcl: %s", fn, err.Error(), err2.Error())
		}
		return cfg, nil
	}
	return cfg, nil
}

type Config struct {
	Group  map[string]Group
	Target map[string]Target
}

func mergeConfig(c1, c2 Config) Config {
	for k, g := range c2.Group {
		if c1.Group == nil {
			c1.Group = map[string]Group{}
		}
		if g1, exists := c1.Group[k]; exists {
		nextTarget:
			for _, t := range g.Targets {
				for _, t2 := range g1.Targets {
					if t == t2 {
						continue nextTarget
					}
				}
				g1.Targets = append(g1.Targets, t)
			}
			c1.Group[k] = g1
		} else {
			c1.Group[k] = g
		}
	}

	for k, t := range c2.Target {
		if c1.Target == nil {
			c1.Target = map[string]Target{}
		}
		if base, ok := c1.Target[k]; ok {
			t = merge(base, t)
		}
		c1.Target[k] = t
	}
	return c1
}

func (c Config) expandTargets(pattern string) ([]string, error) {
	if _, ok := c.Target[pattern]; ok {
		return []string{pattern}, nil
	}
	var names []string
	for name := range c.Target {
		ok, err := path.Match(pattern, name)
		if err != nil {
			return nil, errors.Wrapf(err, "could not match targets with '%s'", pattern)
		}
		if ok {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil, errors.Errorf("could not find any target matching '%s'", pattern)
	}
	return names, nil
}

func (c Config) newOverrides(v []string) (map[string]Target, error) {
	m := map[string]Target{}
	for _, v := range v {

		parts := strings.SplitN(v, "=", 2)
		keys := strings.SplitN(parts[0], ".", 3)
		if len(keys) < 2 {
			return nil, errors.Errorf("invalid override key %s, expected target.name", parts[0])
		}

		pattern := keys[0]
		if len(parts) != 2 && keys[1] != "args" {
			return nil, errors.Errorf("invalid override %s, expected target.name=value", v)
		}

		names, err := c.expandTargets(pattern)
		if err != nil {
			return nil, err
		}

		for _, name := range names {
			t := m[name]

			switch keys[1] {
			case "context":
				t.Context = &parts[1]
			case "dockerfile":
				t.Dockerfile = &parts[1]
			case "args":
				if len(keys) != 3 {
					return nil, errors.Errorf("invalid key %s, args requires name", parts[0])
				}
				if t.Args == nil {
					t.Args = map[string]string{}
				}
				if len(parts) < 2 {
					v, ok := os.LookupEnv(keys[2])
					if ok {
						t.Args[keys[2]] = v
					}
				} else {
					t.Args[keys[2]] = parts[1]
				}
			case "labels":
				if len(keys) != 3 {
					return nil, errors.Errorf("invalid key %s, lanels requires name", parts[0])
				}
				if t.Labels == nil {
					t.Labels = map[string]string{}
				}
				t.Labels[keys[2]] = parts[1]
			case "tags":
				t.Tags = append(t.Tags, parts[1])
			case "cache-from":
				t.CacheFrom = append(t.CacheFrom, parts[1])
			case "cache-to":
				t.CacheTo = append(t.CacheTo, parts[1])
			case "target":
				s := parts[1]
				t.Target = &s
			case "secrets":
				t.Secrets = append(t.Secrets, parts[1])
			case "ssh":
				t.SSH = append(t.SSH, parts[1])
			case "platform":
				t.Platforms = append(t.Platforms, parts[1])
			case "output":
				t.Outputs = append(t.Outputs, parts[1])
			case "no-cache":
				noCache, err := strconv.ParseBool(parts[1])
				if err != nil {
					return nil, errors.Errorf("invalid value %s for boolean key no-cache", parts[1])
				}
				t.NoCache = noCache
			case "pull":
				pull, err := strconv.ParseBool(parts[1])
				if err != nil {
					return nil, errors.Errorf("invalid value %s for boolean key pull", parts[1])
				}
				t.Pull = pull
			default:
				return nil, errors.Errorf("unknown key: %s", keys[1])
			}
			m[name] = t
		}
	}
	return m, nil
}

func (c Config) ResolveGroup(name string) []string {
	return c.group(name, map[string]struct{}{})
}

func (c Config) group(name string, visited map[string]struct{}) []string {
	if _, ok := visited[name]; ok {
		return nil
	}
	g, ok := c.Group[name]
	if !ok {
		return []string{name}
	}
	visited[name] = struct{}{}
	targets := make([]string, 0, len(g.Targets))
	for _, t := range g.Targets {
		targets = append(targets, c.group(t, visited)...)
	}
	return targets
}

func (c Config) ResolveTarget(name string, overrides map[string]Target) (*Target, error) {
	t, err := c.target(name, map[string]struct{}{}, overrides)
	if err != nil {
		return nil, err
	}
	if t.Context == nil {
		s := "."
		t.Context = &s
	}
	if t.Dockerfile == nil {
		s := "Dockerfile"
		t.Dockerfile = &s
	}
	return t, nil
}

func (c Config) target(name string, visited map[string]struct{}, overrides map[string]Target) (*Target, error) {
	if _, ok := visited[name]; ok {
		return nil, nil
	}
	visited[name] = struct{}{}
	t, ok := c.Target[name]
	if !ok {
		return nil, errors.Errorf("failed to find target %s", name)
	}
	var tt Target
	for _, name := range t.Inherits {
		t, err := c.target(name, visited, overrides)
		if err != nil {
			return nil, err
		}
		if t != nil {
			tt = merge(tt, *t)
		}
	}
	t.Inherits = nil
	tt = merge(merge(merge(defaultTarget(), tt), t), overrides[name])
	tt.normalize()
	return &tt, nil
}

type Group struct {
	Targets []string
	// Target // TODO?
}

type Target struct {
	// Inherits is the only field that cannot be overridden with --set
	Inherits []string `json:"inherits,omitempty" hcl:"inherits,omitempty"`

	Context    *string           `json:"context,omitempty" hcl:"context,omitempty"`
	Dockerfile *string           `json:"dockerfile,omitempty" hcl:"dockerfile,omitempty"`
	Args       map[string]string `json:"args,omitempty" hcl:"args,omitempty"`
	Labels     map[string]string `json:"labels,omitempty" hcl:"labels,omitempty"`
	Tags       []string          `json:"tags,omitempty" hcl:"tags,omitempty"`
	CacheFrom  []string          `json:"cache-from,omitempty"  hcl:"cache-from,omitempty"`
	CacheTo    []string          `json:"cache-to,omitempty"  hcl:"cache-to,omitempty"`
	Target     *string           `json:"target,omitempty" hcl:"target,omitempty"`
	Secrets    []string          `json:"secret,omitempty" hcl:"secret,omitempty"`
	SSH        []string          `json:"ssh,omitempty" hcl:"ssh,omitempty"`
	Platforms  []string          `json:"platforms,omitempty" hcl:"platforms,omitempty"`
	Outputs    []string          `json:"output,omitempty" hcl:"output,omitempty"`
	Pull       bool              `json:"pull,omitempty": hcl:"pull,omitempty"`
	NoCache    bool              `json:"no-cache,omitempty": hcl:"no-cache,omitempty"`
	// IMPORTANT: if you add more fields here, do not forget to update newOverrides and README.
}

func (t *Target) normalize() {
	t.Tags = removeDupes(t.Tags)
	t.Secrets = removeDupes(t.Secrets)
	t.SSH = removeDupes(t.SSH)
	t.Platforms = removeDupes(t.Platforms)
	t.CacheFrom = removeDupes(t.CacheFrom)
	t.CacheTo = removeDupes(t.CacheTo)
	t.Outputs = removeDupes(t.Outputs)
}

func TargetsToBuildOpt(m map[string]Target) (map[string]build.Options, error) {
	m2 := make(map[string]build.Options, len(m))
	for k, v := range m {
		bo, err := toBuildOpt(v)
		if err != nil {
			return nil, err
		}
		m2[k] = *bo
	}
	return m2, nil
}

func toBuildOpt(t Target) (*build.Options, error) {
	if v := t.Context; v != nil && *v == "-" {
		return nil, errors.Errorf("context from stdin not allowed in bake")
	}
	if v := t.Dockerfile; v != nil && *v == "-" {
		return nil, errors.Errorf("dockerfile from stdin not allowed in bake")
	}

	contextPath := "."
	if t.Context != nil {
		contextPath = *t.Context
	}
	dockerfilePath := "Dockerfile"
	if t.Dockerfile != nil {
		dockerfilePath = *t.Dockerfile
	}

	if !isRemoteResource(contextPath) && !path.IsAbs(dockerfilePath) {
		dockerfilePath = path.Join(contextPath, dockerfilePath)
	}

	bo := &build.Options{
		Inputs: build.Inputs{
			ContextPath:    contextPath,
			DockerfilePath: dockerfilePath,
		},
		Tags:      t.Tags,
		BuildArgs: t.Args,
		Labels:    t.Labels,
		NoCache:   t.NoCache,
		Pull:      t.Pull,
	}

	platforms, err := platformutil.Parse(t.Platforms)
	if err != nil {
		return nil, err
	}
	bo.Platforms = platforms

	bo.Session = append(bo.Session, authprovider.NewDockerAuthProvider(os.Stderr))

	secrets, err := build.ParseSecretSpecs(t.Secrets)
	if err != nil {
		return nil, err
	}
	bo.Session = append(bo.Session, secrets)

	ssh, err := build.ParseSSHSpecs(t.SSH)
	if err != nil {
		return nil, err
	}
	bo.Session = append(bo.Session, ssh)

	if t.Target != nil {
		bo.Target = *t.Target
	}

	cacheImports, err := build.ParseCacheEntry(t.CacheFrom)
	if err != nil {
		return nil, err
	}
	bo.CacheFrom = cacheImports

	cacheExports, err := build.ParseCacheEntry(t.CacheTo)
	if err != nil {
		return nil, err
	}
	bo.CacheTo = cacheExports

	outputs, err := build.ParseOutputs(t.Outputs)
	if err != nil {
		return nil, err
	}
	bo.Exports = outputs

	return bo, nil
}

func defaultTarget() Target {
	return Target{}
}

func merge(t1, t2 Target) Target {
	if t2.Context != nil {
		t1.Context = t2.Context
	}
	if t2.Dockerfile != nil {
		t1.Dockerfile = t2.Dockerfile
	}
	for k, v := range t2.Args {
		if t1.Args == nil {
			t1.Args = map[string]string{}
		}
		t1.Args[k] = v
	}
	for k, v := range t2.Labels {
		if t1.Labels == nil {
			t1.Labels = map[string]string{}
		}
		t1.Labels[k] = v
	}
	if t2.Tags != nil { // no merge
		t1.Tags = t2.Tags
	}
	if t2.Target != nil {
		t1.Target = t2.Target
	}
	if t2.Secrets != nil { // merge
		t1.Secrets = append(t1.Secrets, t2.Secrets...)
	}
	if t2.SSH != nil { // merge
		t1.SSH = append(t1.SSH, t2.SSH...)
	}
	if t2.Platforms != nil { // no merge
		t1.Platforms = t2.Platforms
	}
	if t2.CacheFrom != nil { // no merge
		t1.CacheFrom = append(t1.CacheFrom, t2.CacheFrom...)
	}
	if t2.CacheTo != nil { // no merge
		t1.CacheTo = t2.CacheTo
	}
	if t2.Outputs != nil { // no merge
		t1.Outputs = t2.Outputs
	}
	t1.Inherits = append(t1.Inherits, t2.Inherits...)
	return t1
}

func removeDupes(s []string) []string {
	i := 0
	seen := make(map[string]struct{}, len(s))
	for _, v := range s {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		s[i] = v
		i++
	}
	return s[:i]
}

func isRemoteResource(str string) bool {
	return urlutil.IsGitURL(str) || urlutil.IsURL(str)
}
