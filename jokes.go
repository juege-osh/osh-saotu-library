package main

import (
	"net/http"
)

func (s *server) browseJokes(w http.ResponseWriter, r *http.Request) {
	data, err := assets.ReadFile("jokes.json")
	if err != nil {
		apiError(w, 500, "段子暂时没读出来")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Write(data)
}
