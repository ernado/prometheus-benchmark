// Binary exporter-cache periodically fetches the target and caches the response.
package main

import (
	"flag"
	"io"
	"log"
	"net/http"
	"sync/atomic"
	"time"
)

func main() {
	var arg struct {
		Target string
		Listen string
		Rate   time.Duration
	}
	flag.StringVar(&arg.Target, "target", "http://localhost:9101/metrics", "target address")
	flag.StringVar(&arg.Listen, "listen", "127.0.0.1:9100", "listen address")
	flag.DurationVar(&arg.Rate, "rate", time.Second*1, "poll rate")
	flag.Parse()

	response := atomic.Pointer[[]byte]{}
	fetch := func() error {
		res, err := http.Get(arg.Target)
		if err != nil {
			return err
		}
		defer func() {
			_ = res.Body.Close()
		}()
		data, err := io.ReadAll(res.Body)
		if err != nil {
			return err
		}
		response.Store(&data)
		return nil
	}
	if err := fetch(); err != nil {
		panic(err)
	}
	go func() {
		// Periodically fetch the target.
		for range time.Tick(arg.Rate) {
			if err := fetch(); err != nil {
				log.Println("fetch error:", err)
			} else {
				log.Println("fetch success")
			}
		}
	}()
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		v := response.Load()
		_, _ = w.Write(*v)
	}
	http.HandleFunc("/", handler)
	if err := http.ListenAndServe(arg.Listen, nil); err != nil {
		panic(err)
	}
}
