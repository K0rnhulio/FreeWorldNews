package main

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

func validArticleURL(u *url.URL) bool {
	return u != nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.User == nil
}

// importHandler extracts a page the user already loaded in their browser.
// It makes no network requests and never executes the supplied page's scripts.
func importHandler(w http.ResponseWriter, r *http.Request) {
	page := readerPage{Bookmarklet: bookmarklet(r)}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Use the import form or the Read with FreeWorldNews bookmark.", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPageBytes+(64<<10))
	defer r.Body.Close()
	err := r.ParseMultipartForm(maxPageBytes + (64 << 10))
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	if err != nil {
		page.Error = "Couldn't import that page. Choose a saved HTML file smaller than 10 MB."
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		renderReader(w, status, page)
		return
	}
	raw := strings.TrimSpace(r.FormValue("url"))
	u, err := url.Parse(raw)
	if err != nil || !validArticleURL(u) {
		page.Error = "Enter the original article's full http or https link."
		renderReader(w, http.StatusBadRequest, page)
		return
	}
	page.Input, page.Original = raw, raw
	b := []byte(r.FormValue("page"))
	if len(b) == 0 {
		file, _, err := r.FormFile("file")
		if err != nil {
			page.Error = "Choose a saved HTML file, or use Read with FreeWorldNews on the open article."
			renderReader(w, http.StatusBadRequest, page)
			return
		}
		defer file.Close()
		b, err = io.ReadAll(io.LimitReader(file, maxPageBytes+1))
		if err != nil {
			page.Error = "Couldn't read the uploaded HTML file."
			renderReader(w, http.StatusBadRequest, page)
			return
		}
	}
	if len(b) > maxPageBytes {
		page.Error = "The saved page is too large. Choose an HTML file smaller than 10 MB."
		renderReader(w, http.StatusRequestEntityTooLarge, page)
		return
	}
	a := extractArticle(b, u)
	if a.Words == 0 || (a.Words < minArticleLen && detectBotWall(b) != "") {
		page.Error = "This file has no readable article, or still contains a browser verification page. Open the original, finish verification and wait for the article to load before importing it."
		renderReader(w, http.StatusUnprocessableEntity, page)
		return
	}
	a.Source = "your browser's saved page"
	page.Article = a
	renderReader(w, http.StatusOK, page)
}
