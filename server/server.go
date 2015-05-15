package server

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jaschaephraim/lrserver"
	"github.com/jpillora/archiver"
	"gopkg.in/fsnotify.v1"
)

//Server is custom file server
type Server struct {
	c        Config
	addr     string
	port     string
	root     string
	hasIndex bool
	fallback *httputil.ReverseProxy
	watcher  *fsnotify.Watcher
	watching map[string]bool
	lr       *lrserver.Server
}

//NewServer creates a new Server
func New(c Config) (*Server, error) {

	port := strconv.Itoa(c.Port)
	s := &Server{
		c:    c,
		port: port,
		addr: c.Host + ":" + port,
	}

	_, err := os.Stat(c.Directory)
	if c.Directory == "" || err != nil {
		return nil, fmt.Errorf("Missing directory: %s", c.Directory)
	}

	if c.PushState {
		s.root = filepath.Join(c.Directory, "index.html")
		if _, err := os.Stat(s.root); err != nil {
			return nil, fmt.Errorf("'%s' is required for pushstate", s.root)

		}
		s.hasIndex = true
	}

	if c.Fallback != "" {
		u, err := url.Parse(c.Fallback)
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(u.Scheme, "http") {
			return nil, fmt.Errorf("Invalid fallback protocol scheme")
		}
		s.fallback = httputil.NewSingleHostReverseProxy(u)
	}

	if c.LiveReload {
		s.lr, _ = lrserver.New("serve-lr", lrserver.DefaultPort)
		discard := log.New(ioutil.Discard, "", 0)
		s.lr.SetErrorLog(discard)
		s.lr.SetStatusLog(discard)
		s.watching = map[string]bool{}
		s.watcher, err = fsnotify.NewWatcher()
		if err != nil {
			return nil, err
		}
	}

	return s, nil
}

func (s *Server) Start() error {

	if s.c.Open {
		go func() {
			time.Sleep(500 * time.Millisecond)
			cmd := exec.Command("open", "http://localhost:"+s.port)
			cmd.Run()
		}()
	}

	if s.c.LiveReload {
		go func() {
			if err := s.lr.ListenAndServe(); err != nil {
				fmt.Printf("LiveReload server closed: %s", err)
			}
		}()
		go func() {
			for {
				event := <-s.watcher.Events
				s.lr.Reload(event.Name)
			}
		}()
	}

	h := http.Handler(http.HandlerFunc(s.serve))

	//insert development middleware
	if !s.c.FastMode {
		h = s.devIntercept(h)
	}

	//logging is enabled
	if !s.c.Quiet {
		fmt.Println(c("serving ", "grey") +
			c(ShortenPath(s.c.Directory), "cyan") +
			c(" on port ", "grey") +
			c(s.port, "cyan"))
	}
	//listen
	return http.ListenAndServe(s.addr, h)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {

	path := r.URL.Path
	//shorthand
	reply := func(c int, msg string) {
		w.WriteHeader(c)
		if msg != "" {
			w.Write([]byte(msg))
		}
	}
	//requested file
	p := filepath.Join(s.c.Directory, path)
	//check file or dir
	isdir := false
	if info, err := os.Stat(p); err == nil {
		//found! -> is file or dir?
		isdir = info.IsDir()
	} else if s.c.PushState && filepath.Ext(p) == "" {
		//missing and pushstate and no ext
		p = s.root //known to exist
		isdir = false
	} else if s.fallback != nil {
		//missing and fallback proxy enabled
		s.fallback.ServeHTTP(w, r)
		return
	} else if dir, ext, ok := archiveRequest(p); ok {
		//missing and archiving enabled, attempt archive
		a, err := archiver.New(ext)
		if err != nil {
			reply(500, err.Error())
			return
		}
		if !s.c.FastMode {
			a.DirMaxSize = 1e9 //must buffer so, 1GB max
		}
		if err := a.AddDir(dir); err != nil {
			reply(500, err.Error())
			return
		}
		if err := a.Close(); err != nil {
			reply(500, err.Error())
			return
		}
		w.Header().Set("Content-Type", mime.TypeByExtension(ext))
		w.WriteHeader(200)
		io.Copy(w, a)
		return
	} else {
		//not found!!
		reply(404, "Not found")
		return
	}

	//force trailing slash
	if isdir && !s.c.NoSlash && !strings.HasSuffix(path, "/") {
		w.Header().Set("Location", path+"/")
		w.WriteHeader(302)
		w.Write([]byte("Redirecting (must use slash for directories)"))
		return
	}

	//optionally use index instead of directory list
	if isdir && !s.c.NoIndex {
		dirindex := filepath.Join(p, "index.html")
		if _, err := os.Stat(dirindex); err == nil {
			p = dirindex
			isdir = false
		}
	}

	//directory list
	if isdir {
		if s.c.NoList {
			reply(403, "Listing not allowed")
			return
		}
		s.dirlist(w, r, p)
		return
	}

	//check file again
	info, err := os.Stat(p)
	if err != nil {
		reply(404, "Not found")
		return
	}

	//stream file
	f, err := os.Open(p)
	if err != nil {
		reply(500, err.Error())
		return
	}

	//add all served file's parent dirs to the watcher
	if s.c.LiveReload {
		dir, _ := filepath.Split(p)
		if _, watching := s.watching[dir]; !watching {
			if err := s.watcher.Add(dir); err == nil {
				s.watching[dir] = true
			}
		}
	}
	//http.ServeContent handles caching and range requests
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}
