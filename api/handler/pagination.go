package handler

import (
	"net/http"
	"strconv"
)

type pagination struct {
	Page    int
	PerPage int
	Offset  int
}

func parsePagination(r *http.Request) pagination {
	p := pagination{Page: 1, PerPage: 50}

	if v, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && v > 0 {
		p.Page = v
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("per_page")); err == nil && v > 0 {
		p.PerPage = v
	}
	if p.PerPage > 200 {
		p.PerPage = 200
	}
	if p.Page > 10000 {
		p.Page = 10000
	}

	p.Offset = (p.Page - 1) * p.PerPage
	return p
}
