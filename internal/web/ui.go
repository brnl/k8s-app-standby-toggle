package web

import (
	"bytes"
	_ "embed"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/brnl/k8s-app-standby-toggle/internal/auth"
	"github.com/brnl/k8s-app-standby-toggle/internal/controller"
)

//go:embed page.html
var pageHTML string

var pageTmpl = template.Must(template.New("page").Funcs(template.FuncMap{
	"iso":   func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
	"clock": func(t time.Time) string { return t.UTC().Format("Mon 15:04 UTC") },
	"short": shortDuration,
	"join":  strings.Join,
	"owners": func(ls []controller.LeaseView) string {
		names := make([]string, len(ls))
		for i, l := range ls {
			names[i] = l.Owner
		}
		return strings.Join(names, ", ")
	},
	"lastExpiry": func(ls []controller.LeaseView) time.Time {
		var t time.Time
		for _, l := range ls {
			if l.Expires.After(t) {
				t = l.Expires
			}
		}
		return t
	},
	"isMissing": func(missing []string, app string) bool {
		for _, m := range missing {
			if m == app {
				return true
			}
		}
		return false
	},
}).Parse(pageHTML))

type pageData struct {
	User          string
	CSRF          string
	AuthDisabled  bool
	Version       string
	Groups        []controller.GroupView
	WakeDurations []time.Duration
	HoldDurations []time.Duration
	HoldDefault   time.Duration
	Status        controller.Status
}

func (s *server) data(p auth.Principal) pageData {
	d := pageData{
		User:          p.Name,
		CSRF:          p.CSRF,
		AuthDisabled:  s.Auth.Disabled,
		Version:       s.Version,
		Groups:        s.Controller.Views(),
		WakeDurations: s.WakeDurations,
		HoldDefault:   s.HoldDefault,
		Status:        s.Controller.Status(),
	}
	// Hold for any wake duration up to the maximum, and the default.
	for _, w := range s.WakeDurations {
		if w <= s.HoldMax && w != s.HoldDefault {
			d.HoldDurations = append(d.HoldDurations, w)
		}
	}
	d.HoldDurations = append(d.HoldDurations, s.HoldDefault)
	return d
}

func (s *server) page(w http.ResponseWriter, r *http.Request) {
	p, ok := s.Auth.Person(r)
	if !ok {
		http.Redirect(w, r, auth.LoginURL("/"), http.StatusFound)
		return
	}
	s.render(w, "page", s.data(p))
}

// fragment serves the groups part of the page, for the self-refresh.
func (s *server) fragment(w http.ResponseWriter, r *http.Request) {
	p, ok := s.Auth.Person(r)
	if !ok {
		http.Error(w, "not signed in", http.StatusUnauthorized)
		return
	}
	s.render(w, "groups", s.data(p))
}

func (s *server) render(w http.ResponseWriter, name string, d pageData) {
	var buf bytes.Buffer
	if err := pageTmpl.ExecuteTemplate(&buf, name, d); err != nil {
		s.Log.Error("rendering the page failed", "error", err.Error())
		http.Error(w, "failed to render the page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}
