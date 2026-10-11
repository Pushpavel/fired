package artifacts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	pa "github.com/Pushpavel/fired/pkg/artifacts"
)

// cleanPath validates a tree path: relative, slash-separated, no "." or
// ".." segments, no empty segments.
func cleanPath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("path is empty")
	}
	if len(p) > 1024 {
		return "", fmt.Errorf("path %q is longer than 1024 bytes", p[:64]+"…")
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("path %q must be relative and use forward slashes", p)
	}
	for _, seg := range strings.Split(strings.TrimSuffix(p, "/"), "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("path %q has an empty, \".\" or \"..\" segment", p)
		}
	}
	return strings.TrimSuffix(p, "/"), nil
}

// under reports whether p is dir itself or inside it.
func under(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// ParseSpec decodes artifact.json strictly: unknown fields are errors, so
// a typo like "schedul" fails at publish instead of being ignored.
func ParseSpec(data []byte) (pa.Spec, error) {
	var s pa.Spec
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return s, fmt.Errorf("%s: %v", pa.SpecFile, err)
	}
	if dec.More() {
		return s, fmt.Errorf("%s: trailing data after the JSON object", pa.SpecFile)
	}
	return s, nil
}

// EncodeSpec renders a spec as artifact.json content.
func EncodeSpec(s pa.Spec) []byte {
	b, _ := json.MarshalIndent(s, "", "  ")
	return append(b, '\n')
}

// viewExts lists the extensions each view type takes; nil takes any file.
var viewExts = map[string][]string{
	pa.ViewPage:     {".html", ".htm"},
	pa.ViewMarkdown: {".md", ".markdown"},
	pa.ViewCSV:      {".csv"},
	pa.ViewJSON:     {".json"},
	pa.ViewCode:     nil,
	pa.ViewSVG:      {".svg"},
	pa.ViewImage:    {".png", ".jpg", ".jpeg", ".gif", ".webp"},
}

var viewTypes = []string{pa.ViewPage, pa.ViewMarkdown, pa.ViewCSV, pa.ViewJSON, pa.ViewCode, pa.ViewSVG, pa.ViewImage}

// ValidateSpec checks a spec against the tree it will be published with.
func ValidateSpec(s *pa.Spec, tree map[string]pa.File) error {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }
	if len(s.Title) > 200 {
		add("title is longer than 200 bytes")
	}
	if len(s.Description) > 2000 {
		add("description is longer than 2000 bytes")
	}
	if v := s.View; v != nil {
		exts, known := viewExts[v.Type]
		if !known {
			add("view.type: %q isn't one of %s", v.Type, strings.Join(viewTypes, ", "))
		}
		if e, err := cleanPath(v.Entry); err != nil {
			add("view.entry: %v", err)
		} else {
			if ext := strings.ToLower(path.Ext(e)); known && exts != nil && !slices.Contains(exts, ext) {
				add("view.entry: a %s view needs a %s file, not %q", v.Type, strings.Join(exts, " or "), v.Entry)
			} else if _, ok := tree[e]; !ok && !(s.Job != nil && isOutput(e)) {
				add("view.entry: %q is not a file in this version (publish it, point the view at another file, or have the job write it to %s/)", v.Entry, pa.OutputDir)
			}
			v.Entry = e
		}
	}
	if s.Job != nil {
		j := *s.Job
		if strings.TrimSpace(j.Run) == "" {
			add("job.run is required")
		}
		if j.Schedule != "" {
			if _, err := ParseCron(j.Schedule, j.Timezone); err != nil {
				add("job: %v", err)
			}
		} else if j.Timezone != "" {
			add("job.timezone is set without a schedule")
		}
		if j.Timeout < 0 || j.ActiveFor < 0 {
			add("job: timeout and active_for must be positive")
		}
		switch j.OnBusy {
		case "":
		case pa.OnBusyLatest, pa.OnBusyQueue:
		default:
			add("job.on_busy must be %q or %q", pa.OnBusyLatest, pa.OnBusyQueue)
		}
		if j.Queue != nil {
			if j.OnBusy != pa.OnBusyQueue {
				add("job.queue is only used with on_busy %q", pa.OnBusyQueue)
			}
			if j.Queue.Max < 0 || j.Queue.MaxAge < 0 {
				add("job.queue: max and max_age must be positive")
			}
		}
		s.Job = &j
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid %s: %s", pa.SpecFile, strings.Join(errs, "; "))
	}
	return nil
}

// jobDefaults fills in the service defaults for unset job fields.
func (c *Config) jobDefaults(j pa.Job) pa.Job {
	if j.Timeout == 0 {
		j.Timeout = c.Limits.DefaultMaxRun
	}
	if j.ActiveFor == 0 {
		j.ActiveFor = c.Limits.DefaultActiveFor
	}
	if j.OnBusy == "" {
		j.OnBusy = pa.OnBusyLatest
	}
	if j.OnBusy == pa.OnBusyQueue {
		q := pa.QueueSpec{}
		if j.Queue != nil {
			q = *j.Queue
		}
		if q.Max == 0 {
			q.Max = c.Limits.QueueMax
		}
		if q.MaxAge == 0 {
			q.MaxAge = c.Limits.QueueMaxAge
		}
		j.Queue = &q
	}
	if j.Template == "" {
		j.Template = c.Runner.Template
	}
	return j
}

// contentType picks a Content-Type for serving a file.
func contentType(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".md", ".markdown":
		return "text/markdown; charset=utf-8"
	case ".csv":
		return "text/csv; charset=utf-8"
	case ".json":
		return "application/json"
	case ".js", ".mjs", ".jsx":
		return "text/javascript; charset=utf-8"
	case ".ts", ".tsx":
		return "text/plain; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".txt", ".log":
		return "text/plain; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".ico":
		return "image/x-icon"
	case ".pdf":
		return "application/pdf"
	case ".wasm":
		return "application/wasm"
	case ".woff2":
		return "font/woff2"
	case ".woff":
		return "font/woff"
	case ".ttf":
		return "font/ttf"
	case ".mp4":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".mp3":
		return "audio/mpeg"
	case ".xml":
		return "application/xml"
	case ".yaml", ".yml", ".toml", ".py", ".go", ".sh", ".rs":
		return "text/plain; charset=utf-8"
	}
	return ""
}

func durStr(d pa.Duration) string { return time.Duration(d).String() }
