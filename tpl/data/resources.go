// Copyright 2016 The Hugo Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package data

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io/ioutil"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spf13/afero"
	"github.com/spf13/hugo/config"
	"github.com/spf13/hugo/helpers"
	jww "github.com/spf13/jwalterweatherman"
)

var (
	remoteURLLock = &remoteLock{m: make(map[string]*sync.Mutex)}
	resSleep      = time.Second * 2 // if JSON decoding failed sleep for n seconds before retrying
	resRetries    = 1               // number of retries to load the JSON from URL or local file system
)

type remoteLock struct {
	sync.RWMutex
	m map[string]*sync.Mutex
}

// URLLock locks an URL during download
func (l *remoteLock) URLLock(url string) {
	var (
		lock *sync.Mutex
		ok   bool
	)
	l.Lock()
	if lock, ok = l.m[url]; !ok {
		lock = &sync.Mutex{}
		l.m[url] = lock
	}
	l.Unlock()
	lock.Lock()
}

// URLUnlock unlocks an URL when the download has been finished. Use only in defer calls.
func (l *remoteLock) URLUnlock(url string) {
	l.RLock()
	defer l.RUnlock()
	if um, ok := l.m[url]; ok {
		um.Unlock()
	}
}

// getRemote loads the content of a remote file. This method is thread safe.
func getRemote(url string, fs afero.Fs, cfg config.Provider, hc *http.Client) ([]byte, error) {
	c, err := getCache(url, fs, cfg, cfg.GetBool("ignoreCache"))
	if c != nil && err == nil {
		return c, nil
	}
	if err != nil {
		return nil, err
	}

	// avoid race condition with locks, block other goroutines if the current url is processing
	remoteURLLock.URLLock(url)
	defer func() { remoteURLLock.URLUnlock(url) }()

	// avoid multiple locks due to calling getCache twice
	c, err = getCache(url, fs, cfg, cfg.GetBool("ignoreCache"))
	if c != nil && err == nil {
		return c, nil
	}
	if err != nil {
		return nil, err
	}

	jww.INFO.Printf("Downloading: %s ...", url)
	res, err := hc.Get(url)
	if err != nil {
		return nil, err
	}
	c, err = ioutil.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		return nil, err
	}
	err = writeCache(url, c, fs, cfg, cfg.GetBool("ignoreCache"))
	if err != nil {
		return nil, err
	}
	jww.INFO.Printf("... and cached to: %s", getCacheFileID(cfg, url))
	return c, nil
}

// getLocal loads the content of a local file
func getLocal(url string, fs afero.Fs, cfg config.Provider) ([]byte, error) {
	filename := filepath.Join(cfg.GetString("workingDir"), url)
	if e, err := helpers.Exists(filename, fs); !e {
		return nil, err
	}

	return afero.ReadFile(fs, filename)

}

// getResource loads the content of a local or remote file
func (ns *Namespace) getResource(url string) ([]byte, error) {
	if url == "" {
		return nil, nil
	}
	if strings.Contains(url, "://") {
		return getRemote(url, ns.deps.Fs.Source, ns.deps.Cfg, http.DefaultClient)
	}
	return getLocal(url, ns.deps.Fs.Source, ns.deps.Cfg)
}

// GetJSON expects one or n-parts of a URL to a resource which can either be a local or a remote one.
// If you provide multiple parts they will be joined together to the final URL.
// GetJSON returns nil or parsed JSON to use in a short code.
func (ns *Namespace) GetJSON(urlParts ...string) interface{} {
	var v interface{}
	url := strings.Join(urlParts, "")

	for i := 0; i <= resRetries; i++ {
		c, err := ns.getResource(url)
		if err != nil {
			jww.ERROR.Printf("Failed to get json resource %s with error message %s", url, err)
			return nil
		}

		err = json.Unmarshal(c, &v)
		if err != nil {
			jww.ERROR.Printf("Cannot read json from resource %s with error message %s", url, err)
			jww.ERROR.Printf("Retry #%d for %s and sleeping for %s", i, url, resSleep)
			time.Sleep(resSleep)
			deleteCache(url, ns.deps.Fs.Source, ns.deps.Cfg)
			continue
		}
		break
	}
	return v
}

// parseCSV parses bytes of CSV data into a slice slice string or an error
func parseCSV(c []byte, sep string) ([][]string, error) {
	if len(sep) != 1 {
		return nil, errors.New("Incorrect length of csv separator: " + sep)
	}
	b := bytes.NewReader(c)
	r := csv.NewReader(b)
	rSep := []rune(sep)
	r.Comma = rSep[0]
	r.FieldsPerRecord = 0
	return r.ReadAll()
}

// GetCSV expects a data separator and one or n-parts of a URL to a resource which
// can either be a local or a remote one.
// The data separator can be a comma, semi-colon, pipe, etc, but only one character.
// If you provide multiple parts for the URL they will be joined together to the final URL.
// GetCSV returns nil or a slice slice to use in a short code.
func (ns *Namespace) GetCSV(sep string, urlParts ...string) [][]string {
	var d [][]string
	url := strings.Join(urlParts, "")

	var clearCacheSleep = func(i int, u string) {
		jww.ERROR.Printf("Retry #%d for %s and sleeping for %s", i, url, resSleep)
		time.Sleep(resSleep)
		deleteCache(url, ns.deps.Fs.Source, ns.deps.Cfg)
	}

	for i := 0; i <= resRetries; i++ {
		c, err := ns.getResource(url)

		if err == nil && !bytes.Contains(c, []byte(sep)) {
			err = errors.New("Cannot find separator " + sep + " in CSV.")
		}

		if err != nil {
			jww.ERROR.Printf("Failed to read csv resource %s with error message %s", url, err)
			clearCacheSleep(i, url)
			continue
		}

		if d, err = parseCSV(c, sep); err != nil {
			jww.ERROR.Printf("Failed to parse csv file %s with error message %s", url, err)
			clearCacheSleep(i, url)
			continue
		}
		break
	}
	return d
}