// A static, dependency-free HTTP workload for the Modify App VM regression.
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
)

func main() {
	marker, err := os.ReadFile("/image-marker")
	if err != nil {
		panic(err)
	}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, string(marker))
	})
	http.HandleFunc("/sentinel", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			data, err := io.ReadAll(io.LimitReader(r.Body, 128))
			if err != nil || len(data) == 0 {
				http.Error(w, "invalid sentinel", 400)
				return
			}
			if err := os.WriteFile("/data/sentinel", data, 0600); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
		}
		data, err := os.ReadFile("/data/sentinel")
		if err != nil {
			http.Error(w, err.Error(), 404)
			return
		}
		fmt.Fprint(w, string(data))
	})
	port := os.Getenv("FIXTURE_PORT")
	if port == "" {
		port = "8080"
	}
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		panic(err)
	}
}
