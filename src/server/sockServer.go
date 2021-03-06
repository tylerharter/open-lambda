package server

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/open-lambda/open-lambda/ol/common"
	"github.com/open-lambda/open-lambda/ol/sandbox"
)

type Handler func(http.ResponseWriter, []string, map[string]interface{}) error

var nextScratchId int64 = 1000

// SOCKServer is a worker server that listens to run lambda requests and forward
// these requests to its sandboxes.
type SOCKServer struct {
	cachePool   *sandbox.SOCKPool
	handlerPool *sandbox.SOCKPool
	sandboxes   sync.Map
}

func (s *SOCKServer) GetSandbox(id string) sandbox.Sandbox {
	val, ok := s.sandboxes.Load(id)
	if !ok {
		return nil
	}
	return val.(sandbox.Sandbox)
}

func (s *SOCKServer) Create(w http.ResponseWriter, rsrc []string, args map[string]interface{}) error {
	// leaves are only in handler pool
	var pool *sandbox.SOCKPool

	var leaf bool
	if b, ok := args["leaf"]; !ok || b.(bool) {
		pool = s.handlerPool
		leaf = true
	} else {
		pool = s.cachePool
		leaf = false
	}

	// create args
	codeDir := args["code"].(string)

	var parent sandbox.Sandbox = nil
	if p, ok := args["parent"]; ok && p != "" {
		parent = s.GetSandbox(p.(string))
		if parent == nil {
			return fmt.Errorf("no sandbox found with ID '%s'", p)
		}
	}

	// spin it up
	scratchId := fmt.Sprintf("dir-%d", atomic.AddInt64(&nextScratchId, 1))
	scratchDir := filepath.Join(common.Conf.Worker_dir, "scratch", scratchId)
	if err := os.MkdirAll(scratchDir, 0777); err != nil {
		panic(err)
	}
	c, err := pool.Create(parent, leaf, codeDir, scratchDir, nil)
	if err != nil {
		return err
	}
	s.sandboxes.Store(c.ID(), c)
	log.Printf("Save ID '%s' to map\n", c.ID())

	w.Write([]byte(fmt.Sprintf("%v\n", c.ID())))
	return nil
}

func (s *SOCKServer) Destroy(w http.ResponseWriter, rsrc []string, args map[string]interface{}) error {
	c := s.GetSandbox(rsrc[0])
	if c == nil {
		return fmt.Errorf("no sandbox found with ID '%s'", rsrc[0])
	}

	c.Destroy()

	return nil
}

func (s *SOCKServer) Pause(w http.ResponseWriter, rsrc []string, args map[string]interface{}) error {
	c := s.GetSandbox(rsrc[0])
	if c == nil {
		return fmt.Errorf("no sandbox found with ID '%s'", rsrc[0])
	}

	return c.Pause()
}

func (s *SOCKServer) Unpause(w http.ResponseWriter, rsrc []string, args map[string]interface{}) error {
	c := s.GetSandbox(rsrc[0])
	if c == nil {
		return fmt.Errorf("no sandbox found with ID '%s'", rsrc[0])
	}

	return c.Unpause()
}

func (s *SOCKServer) Debug(w http.ResponseWriter, rsrc []string, args map[string]interface{}) error {
	str := fmt.Sprintf(
		"========\nCACHE SANDBOXES\n========\n%s========\nHANDLER SANDBOXES\n========\n%s",
		s.cachePool.DebugString(), s.handlerPool.DebugString())
	fmt.Printf("%s\n", str)
	w.Write([]byte(str))
	return nil
}

func (s *SOCKServer) HandleInternal(w http.ResponseWriter, r *http.Request) error {
	log.Printf("%s %s", r.Method, r.URL.Path)

	defer r.Body.Close()

	if r.Method != "POST" {
		return fmt.Errorf("Only POST allowed (found %s)", r.Method)
	}

	rbody, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return err
	}

	var args map[string]interface{}

	if len(rbody) > 0 {
		if err := json.Unmarshal(rbody, &args); err != nil {
			return err
		}
	}

	log.Printf("Parsed Args: %v", args)

	rsrc := strings.Split(r.URL.Path, "/")
	if len(rsrc) < 2 {
		return fmt.Errorf("no path arguments provided in URL")
	}

	routes := map[string]Handler{
		"create":  s.Create,
		"destroy": s.Destroy,
		"pause":   s.Pause,
		"unpause": s.Unpause,
		"debug":   s.Debug,
	}

	if h, ok := routes[rsrc[1]]; ok {
		return h(w, rsrc[2:], args)
	} else {
		return fmt.Errorf("unknown op %s", rsrc[1])
	}
}

func (s *SOCKServer) Handle(w http.ResponseWriter, r *http.Request) {
	if err := s.HandleInternal(w, r); err != nil {
		log.Printf("Request Handler Failed: %v", err)
		w.WriteHeader(500)
		w.Write([]byte(fmt.Sprintf("%v\n", err)))
	}
}

func (s *SOCKServer) cleanup() {
	s.sandboxes.Range(func(key, val interface{}) bool {
		val.(sandbox.Sandbox).Destroy()
		return true
	})
	s.cachePool.Cleanup()
	s.handlerPool.Cleanup()
}

// NewSOCKServer creates a server based on the passed config."
func NewSOCKServer() (*SOCKServer, error) {
	log.Printf("Start SOCK Server")

	cacheMem := sandbox.NewMemPool("sock-cache", common.Conf.Import_cache_mb)
	cache, err := sandbox.NewSOCKPool("sock-cache", cacheMem)
	if err != nil {
		return nil, err
	}

	handlerMem := sandbox.NewMemPool("sock-handlers", common.Conf.Handler_cache_mb)
	handler, err := sandbox.NewSOCKPool("sock-handlers", handlerMem)
	if err != nil {
		return nil, err
	}

	server := &SOCKServer{
		cachePool:   cache,
		handlerPool: handler,
	}

	http.HandleFunc("/", server.Handle)

	return server, nil
}
