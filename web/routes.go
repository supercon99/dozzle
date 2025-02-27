package web

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"sort"
	"time"

	"net/http"
	"os"
	"path"
	"strings"

	"github.com/amir20/dozzle/analytics"
	"github.com/amir20/dozzle/docker"
	"github.com/docker/docker/api/types"
	"github.com/go-chi/chi/v5"

	log "github.com/sirupsen/logrus"
)

// Config is a struct for configuring the web service
type Config struct {
	Base        string
	Addr        string
	Version     string
	Username    string
	Password    string
	Hostname    string
	NoAnalytics bool
	Dev         bool
}

type handler struct {
	clients map[string]DockerClient
	content fs.FS
	config  *Config
}

// Client is a proxy around the docker client
type DockerClient interface {
	ListContainers() ([]docker.Container, error)
	FindContainer(string) (docker.Container, error)
	ContainerLogs(context.Context, string, string, docker.StdType) (io.ReadCloser, error)
	Events(context.Context, chan<- docker.ContainerEvent) <-chan error
	ContainerLogsBetweenDates(context.Context, string, time.Time, time.Time, docker.StdType) (io.ReadCloser, error)
	ContainerStats(context.Context, string, chan<- docker.ContainerStat) error
	Ping(context.Context) (types.Ping, error)
	Host() *docker.Host
}

func CreateServer(clients map[string]DockerClient, content fs.FS, config Config) *http.Server {
	handler := &handler{
		clients: clients,
		content: content,
		config:  &config,
	}
	return &http.Server{Addr: config.Addr, Handler: createRouter(handler)}
}

var fileServer http.Handler

func createRouter(h *handler) *chi.Mux {
	initializeAuth(h)

	base := h.config.Base
	r := chi.NewRouter()

	if !h.config.Dev {
		r.Use(cspHeaders)
	}

	r.Route(base, func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(authorizationRequired)
			r.Get("/api/logs/stream/{host}/{id}", h.streamLogs)
			r.Get("/api/logs/download/{host}/{id}", h.downloadLogs)
			r.Get("/api/logs/{host}/{id}", h.fetchLogsBetweenDates)
			r.Get("/api/events/stream", h.streamEvents)
			r.Get("/logout", h.clearSession)
			r.Get("/version", h.version)
		})

		r.Post("/api/validateCredentials", h.validateCredentials)
		r.Get("/healthcheck", h.healthcheck)
		defaultHandler := http.StripPrefix(strings.Replace(base+"/", "//", "/", 1), http.HandlerFunc(h.index))
		r.Get("/*", func(w http.ResponseWriter, req *http.Request) {
			defaultHandler.ServeHTTP(w, req)
		})
	})

	if base != "/" {
		r.Get(base, func(w http.ResponseWriter, req *http.Request) {
			http.Redirect(w, req, base+"/", http.StatusMovedPermanently)
		})
	}

	fileServer = http.FileServer(http.FS(h.content))

	return r
}

func (h *handler) index(w http.ResponseWriter, req *http.Request) {
	_, err := h.content.Open(req.URL.Path)
	if err == nil && req.URL.Path != "" && req.URL.Path != "/" {
		fileServer.ServeHTTP(w, req)
		if !h.config.NoAnalytics {
			go func() {
				host, _ := os.Hostname()

				var client DockerClient
				for _, v := range h.clients {
					client = v
					break
				}

				if containers, err := client.ListContainers(); err == nil {
					totalContainers := len(containers)
					runningContainers := 0
					for _, container := range containers {
						if container.State == "running" {
							runningContainers++
						}
					}

					re := analytics.RequestEvent{
						ClientId:          host,
						TotalContainers:   totalContainers,
						RunningContainers: runningContainers,
					}
					analytics.SendRequestEvent(re)
				}
			}()
		}
	} else {
		if !isAuthorized(req) && req.URL.Path != "login" {
			http.Redirect(w, req, path.Clean(h.config.Base+"/login"), http.StatusTemporaryRedirect)
			return
		}
		h.executeTemplate(w, req)
	}
}

func (h *handler) executeTemplate(w http.ResponseWriter, req *http.Request) {
	file, err := h.content.Open("index.html")
	if err != nil {
		log.Panic(err)
	}
	bytes, err := io.ReadAll(file)
	if err != nil {
		log.Panic(err)
	}
	tmpl, err := template.New("index.html").Funcs(template.FuncMap{
		"marshal": func(v interface{}) template.JS {
			a, _ := json.Marshal(v)
			return template.JS(a)
		},
	}).Parse(string(bytes))
	if err != nil {
		log.Panic(err)
	}

	path := ""
	if h.config.Base != "/" {
		path = h.config.Base
	}

	hosts := make([]*docker.Host, 0, len(h.clients))
	for _, v := range h.clients {
		hosts = append(hosts, v.Host())
	}
	sort.Slice(hosts, func(i, j int) bool {
		return hosts[i].Name < hosts[j].Name
	})

	config := map[string]interface{}{
		"base":                path,
		"version":             h.config.Version,
		"authorizationNeeded": h.isAuthorizationNeeded(req),
		"secured":             secured,
		"hostname":            h.config.Hostname,
		"hosts":               hosts,
	}

	data := map[string]interface{}{
		"Config":   config,
		"Dev":      h.config.Dev,
		"Manifest": h.readManifest(),
		"Base":     path,
	}

	err = tmpl.Execute(w, data)
	if err != nil {
		log.Panic(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (h *handler) readManifest() map[string]interface{} {
	if h.config.Dev {
		return map[string]interface{}{}
	} else {
		file, err := h.content.Open("manifest.json")
		if err != nil {
			// this should only happen during test. In production, the file is embedded in the binary and checked in main.go
			return map[string]interface{}{}
		}
		bytes, err := io.ReadAll(file)
		if err != nil {
			log.Fatalf("Could not read manifest.json: %v", err)
		}
		var manifest map[string]interface{}
		err = json.Unmarshal(bytes, &manifest)
		if err != nil {
			log.Fatalf("Could not parse manifest.json: %v", err)
		}
		return manifest
	}

}

func (h *handler) version(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Content-Type", "text/html")
	fmt.Fprintf(w, "<pre>%v</pre>", h.config.Version)
}

func (h *handler) healthcheck(w http.ResponseWriter, r *http.Request) {
	log.Trace("Executing healthcheck request")
	var client DockerClient
	for _, v := range h.clients {
		client = v
		break
	}

	if ping, err := client.Ping(r.Context()); err != nil {
		log.Error(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	} else {
		fmt.Fprintf(w, "OK API Version %v", ping.APIVersion)
	}
}

func (h *handler) clientFromRequest(r *http.Request) DockerClient {
	host := chi.URLParam(r, "host")

	if host == "" {
		log.Fatalf("No host found for url %v", r.URL)
	}

	if client, ok := h.clients[host]; ok {
		return client
	}

	log.Fatalf("No client found for host %v and url %v", host, r.URL)
	return nil
}
