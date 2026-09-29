// Package egg reads Pterodactyl and Pelican egg files, the game server
// definitions that most existing panels share, into one internal format.
package egg

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// Egg is one game server definition.
type Egg struct {
	Version     string // meta.version of the file it came from
	Name        string
	Author      string
	Description string
	Features    []string
	Images      []Image // the first one is the default
	// Startup is the default command; StartupCommands has every named
	// variant in file order, and Startup is the first of them.
	Startup         string
	StartupCommands []Command
	// Stop is the raw value: a console command, or "^" plus a signal.
	Stop      string
	Done      []string // console lines that mean the server has started
	Files     []ConfigFile
	Install   Install
	Variables []Variable
}

// FeatureEULA marks games, Minecraft's, that will not start until the
// player has accepted a license by writing eula=true to eula.txt.
const FeatureEULA = "eula"

// HasFeature reports whether the egg lists the feature, such as "eula".
func (e *Egg) HasFeature(name string) bool {
	return slices.Contains(e.Features, name)
}

type Image struct {
	Label string
	Ref   string
}

type Command struct {
	Label string
	Line  string
}

type Install struct {
	Image      string
	Entrypoint string
	Script     string
}

type Variable struct {
	Name         string
	Description  string
	Env          string
	Default      string
	UserViewable bool
	UserEditable bool
	Rules        []string
}

// ConfigFile is a file the panel edits before each start.
type ConfigFile struct {
	Path   string
	Parser string // properties, file, yaml, json, ini or xml
	Find   []Replace
}

// Replace sets Key to Value. Value may hold placeholders. When IfValue is
// set the key is only changed if it currently holds that value; eggs write
// that as a nested object under the key.
type Replace struct {
	Key     string
	Value   string
	IfValue string
}

var envName = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`) })

var versions = map[string]bool{
	"PTDL_v1": true, "PTDL_v2": true,
	"PLCN_v1": true, "PLCN_v2": true, "PLCN_v3": true,
}

// Parse reads an egg in any supported format. The format is told apart by
// its first character and then by meta.version.
func Parse(data []byte) (*Egg, error) {
	root, err := decode(data)
	if err != nil {
		return nil, fmt.Errorf("read egg: %w", err)
	}
	var version string
	if meta, ok := root.get("meta").(*object); ok {
		version = text(meta.get("version"))
	}
	if !versions[version] {
		if version == "" {
			return nil, errors.New("egg has no meta.version")
		}
		return nil, fmt.Errorf("unsupported egg version %q", version)
	}

	e := &Egg{
		Version:     version,
		Name:        strings.TrimSpace(text(root.get("name"))),
		Author:      text(root.get("author")),
		Description: text(root.get("description")),
		Features:    stringList(root.get("features")),
	}
	e.Images = images(root)
	e.StartupCommands = startups(root)
	if len(e.StartupCommands) > 0 {
		e.Startup = e.StartupCommands[0].Line
	}

	config, err := embedded(root.get("config"))
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	e.Stop = strings.TrimSpace(text(config.get("stop")))
	startup, err := embedded(config.get("startup"))
	if err != nil {
		return nil, fmt.Errorf("config.startup: %w", err)
	}
	e.Done = stringList(startup.get("done"))
	files, err := embedded(config.get("files"))
	if err != nil {
		return nil, fmt.Errorf("config.files: %w", err)
	}
	if e.Files, err = configFiles(files); err != nil {
		return nil, err
	}

	if scripts, ok := root.get("scripts").(*object); ok {
		if in, ok := scripts.get("installation").(*object); ok {
			e.Install = Install{
				Image:      text(in.get("container")),
				Entrypoint: text(in.get("entrypoint")),
				Script:     text(in.get("script")),
			}
		}
	}

	vars, _ := root.get("variables").([]any)
	for i, raw := range vars {
		o, ok := raw.(*object)
		if !ok {
			return nil, fmt.Errorf("variable %d is not an object", i+1)
		}
		v := Variable{
			Name:         text(o.get("name")),
			Description:  text(o.get("description")),
			Env:          text(o.get("env_variable")),
			Default:      text(o.get("default_value")),
			UserViewable: boolean(o.get("user_viewable")),
			UserEditable: boolean(o.get("user_editable")),
		}
		if !envName().MatchString(v.Env) {
			return nil, fmt.Errorf("variable %d (%q) has no usable env_variable", i+1, v.Name)
		}
		v.Rules = rules(o.get("rules"))
		e.Variables = append(e.Variables, v)
	}

	switch {
	case e.Name == "":
		return nil, errors.New("egg has no name")
	case len(e.Images) == 0:
		return nil, errors.New("egg has no docker image")
	case strings.TrimSpace(e.Startup) == "":
		return nil, errors.New("egg has no startup command")
	}
	return e, nil
}

// images reads docker_images (label to reference). PTDL_v1 has a single
// "image" or a plain "images" list, where the reference is its own label.
func images(root *object) []Image {
	var out []Image
	if m, ok := root.get("docker_images").(*object); ok {
		for _, label := range m.keys {
			if ref := text(m.vals[label]); ref != "" {
				out = append(out, Image{label, ref})
			}
		}
	}
	if len(out) > 0 {
		return out
	}
	list := stringList(root.get("images"))
	if len(list) == 0 {
		list = stringList(root.get("image"))
	}
	for _, ref := range list {
		out = append(out, Image{ref, ref})
	}
	return out
}

// startups reads startup_commands (PLCN_v3) or the single startup string.
func startups(root *object) []Command {
	var out []Command
	if m, ok := root.get("startup_commands").(*object); ok {
		for _, label := range m.keys {
			if line := text(m.vals[label]); line != "" {
				out = append(out, Command{label, line})
			}
		}
	}
	if len(out) == 0 {
		if line := text(root.get("startup")); line != "" {
			out = append(out, Command{"Default", line})
		}
	}
	return out
}

func rules(v any) []string {
	if s, ok := v.(string); ok {
		var out []string
		for _, r := range strings.Split(s, "|") {
			if r = strings.TrimSpace(r); r != "" {
				out = append(out, r)
			}
		}
		return out
	}
	return stringList(v)
}

func configFiles(files *object) ([]ConfigFile, error) {
	if files == nil {
		return nil, nil
	}
	var out []ConfigFile
	for _, path := range files.keys {
		o, ok := files.vals[path].(*object)
		if !ok {
			return nil, fmt.Errorf("config file %q is not an object", path)
		}
		f := ConfigFile{Path: path, Parser: text(o.get("parser"))}
		if find, ok := o.get("find").(*object); ok {
			for _, key := range find.keys {
				if cond, ok := find.vals[key].(*object); ok {
					for _, ifValue := range cond.keys {
						f.Find = append(f.Find, Replace{key, text(cond.vals[ifValue]), ifValue})
					}
					continue
				}
				f.Find = append(f.Find, Replace{Key: key, Value: text(find.vals[key])})
			}
		}
		out = append(out, f)
	}
	return out, nil
}

// StopSignal reports whether Stop is a signal rather than a console
// command. Eggs write it as "^C" for SIGINT or "^" plus a signal name.
func (e *Egg) StopSignal() (name string, ok bool) {
	rest, found := strings.CutPrefix(e.Stop, "^")
	if !found || rest == "" {
		return "", false
	}
	rest = strings.ToUpper(rest)
	if rest == "C" {
		return "SIGINT", true
	}
	if !strings.HasPrefix(rest, "SIG") {
		rest = "SIG" + rest
	}
	return rest, true
}
