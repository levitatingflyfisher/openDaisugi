package plugins

import (
	"io/fs"
	"path/filepath"

	"github.com/opendaisugi/coppice/internal/config"
	"github.com/opendaisugi/coppice/internal/web"
	shipped "github.com/opendaisugi/coppice/plugins"
)

// UserDir is where the operator's own plugins live:
// $XDG_CONFIG_HOME/coppice/plugins, beside coppice.toml.
func UserDir() string {
	return filepath.Join(filepath.Dir(config.Path()), "plugins")
}

// Sources is the shipped plugins, then the operator's own. The operator's
// directory wins for an id both have.
func Sources() []Source {
	return []Source{{FS: shipped.Files(), Shipped: true}, {Dir: UserDir()}}
}

// SharedLib is the library the shipped policies import, or nil when the
// binary carries none.
func SharedLib() fs.FS {
	sub, err := fs.Sub(shipped.Files(), "_lib")
	if err != nil {
		return nil
	}
	if _, err := fs.Stat(sub, "."); err != nil {
		return nil
	}
	return sub
}

// LoadEnabled loads the enabled ids from Sources.
func LoadEnabled(enabled []string) ([]Plugin, []Problem) {
	return LoadFrom(Sources(), enabled)
}

// ViewsOf is the views among ps, as the web server serves them.
func ViewsOf(ps []Plugin) []web.View {
	var out []web.View
	for _, p := range ps {
		if p.Kind != KindView {
			continue
		}
		title := p.Title
		if title == "" {
			title = p.ID
		}
		out = append(out, web.View{ID: p.ID, Title: title, Page: p.Page, FS: p.FS, Ring: p.Ring})
	}
	return out
}
