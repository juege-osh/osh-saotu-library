package main

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"
)

// Share previews are rendered only for published images, so crawlers never see pending uploads.
func (s *server) indexWithSharePreview(r *http.Request, page []byte) []byte {
	title := "每日骚图 · 今天也要笑一下"
	description := "每日吊图、搞图、骚图和段子。按场景找图，分享好图。"
	pageURL := "https://saotu.osh.lol/"
	imageURL := "https://saotu.osh.lol/hero.png"
	id, err := strconv.ParseInt(r.URL.Query().Get("image"), 10, 64)
	if err == nil && id > 0 {
		items, _, readErr := s.imageList("i.status='approved' and i.id=?", []any{id}, 1)
		if readErr == nil && len(items) > 0 {
			item := items[0]
			title = item.Title + " · 每日骚图"
			description = item.Scene
			if description == "" {
				description = item.Description
			}
			runes := []rune(description)
			if len(runes) > 180 {
				description = string(runes[:180])
			}
			pageURL = fmt.Sprintf("https://saotu.osh.lol/?image=%d#library", id)
			imageURL = fmt.Sprintf("https://saotu.osh.lol/api/library/previews/%d", id)
		}
	}
	if pageURL == "https://saotu.osh.lol/" {
		jokeID, jokeErr := strconv.Atoi(r.URL.Query().Get("joke"))
		if jokeErr == nil && jokeID > 0 {
			var jokes []struct {
				ID   int    `json:"id"`
				Text string `json:"text"`
			}
			if data, readErr := assets.ReadFile("jokes.json"); readErr == nil && json.Unmarshal(data, &jokes) == nil {
				for _, joke := range jokes {
					if joke.ID == jokeID {
						title = "来一条段子 · 每日骚图"
						description = joke.Text
						pageURL = fmt.Sprintf("https://saotu.osh.lol/?joke=%d#jokes", jokeID)
						break
					}
				}
			}
		}
	}
	meta := fmt.Sprintf(`<meta property="og:type" content="website"><meta property="og:site_name" content="每日骚图"><meta property="og:title" content="%s"><meta property="og:description" content="%s"><meta property="og:image" content="%s"><meta property="og:url" content="%s"><meta name="twitter:card" content="summary_large_image">`, html.EscapeString(title), html.EscapeString(description), html.EscapeString(imageURL), html.EscapeString(pageURL))
	return []byte(strings.Replace(string(page), "</head>", meta+"</head>", 1))
}
