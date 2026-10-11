package artifacts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	pa "github.com/Pushpavel/fired/pkg/artifacts"
)

var jobNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,47}$`)

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
	if s.Page != "" {
		if p, err := cleanPath(s.Page); err != nil {
			add("page: %v", err)
		} else {
			if ext := strings.ToLower(path.Ext(p)); ext != ".html" && ext != ".htm" {
				add("page: %q must be an .html file", s.Page)
			} else if _, ok := tree[p]; !ok {
				add("page: %q is not a file in this version (publish it, or set \"page\" to the file to open)", s.Page)
			}
			s.Page = p
		}
	}
	names := make([]string, 0, len(s.Jobs))
	for n := range s.Jobs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		j := s.Jobs[n]
		if !jobNameRE.MatchString(n) {
			add("jobs: name %q must match %s", n, jobNameRE)
		}
		if strings.TrimSpace(j.Run) == "" {
			add("jobs.%s.run is required", n)
		}
		for k, w := range j.Writes {
			c, err := cleanPath(w)
			if err != nil {
				add("jobs.%s.writes[%d]: %v", n, k, err)
				continue
			}
			if c == pa.SpecFile {
				add("jobs.%s.writes[%d]: a job can't write %s", n, k, pa.SpecFile)
			}
			j.Writes[k] = c
		}
		if j.State != "" {
			c, err := cleanPath(j.State)
			if err != nil {
				add("jobs.%s.state: %v", n, err)
			} else {
				for _, w := range j.Writes {
					if under(c, w) || under(w, c) {
						add("jobs.%s.state %q overlaps writes path %q", n, c, w)
					}
				}
				for p := range tree {
					if under(p, c) {
						add("jobs.%s.state %q must not contain published files (found %q)", n, c, p)
						break
					}
				}
				j.State = c
			}
		}
		if j.Schedule != "" {
			if _, err := ParseCron(j.Schedule, j.Timezone); err != nil {
				add("jobs.%s: %v", n, err)
			}
		} else if j.Timezone != "" {
			add("jobs.%s.timezone is set without a schedule", n)
		}
		if j.Timeout < 0 || j.ActiveFor < 0 {
			add("jobs.%s: timeout and active_for must be positive", n)
		}
		switch j.OnBusy {
		case "":
		case pa.OnBusyLatest, pa.OnBusyQueue:
		default:
			add("jobs.%s.on_busy must be %q or %q", n, pa.OnBusyLatest, pa.OnBusyQueue)
		}
		if j.Queue != nil {
			if j.OnBusy != pa.OnBusyQueue {
				add("jobs.%s.queue is only used with on_busy %q", n, pa.OnBusyQueue)
			}
			if j.Queue.Max < 0 || j.Queue.MaxAge < 0 {
				add("jobs.%s.queue: max and max_age must be positive", n)
			}
		}
		s.Jobs[n] = j
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
