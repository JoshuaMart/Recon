package handler

import (
	"net/http/httptest"
	"testing"
)

func TestParsePagination_Defaults(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	pg := parsePagination(req)

	if pg.Page != 1 {
		t.Errorf("expected page 1, got %d", pg.Page)
	}
	if pg.PerPage != 50 {
		t.Errorf("expected per_page 50, got %d", pg.PerPage)
	}
	if pg.Offset != 0 {
		t.Errorf("expected offset 0, got %d", pg.Offset)
	}
}

func TestParsePagination_CustomValues(t *testing.T) {
	req := httptest.NewRequest("GET", "/?page=3&per_page=100", nil)
	pg := parsePagination(req)

	if pg.Page != 3 {
		t.Errorf("expected page 3, got %d", pg.Page)
	}
	if pg.PerPage != 100 {
		t.Errorf("expected per_page 100, got %d", pg.PerPage)
	}
	if pg.Offset != 200 {
		t.Errorf("expected offset 200, got %d", pg.Offset)
	}
}

func TestParsePagination_MaxPerPage(t *testing.T) {
	req := httptest.NewRequest("GET", "/?per_page=500", nil)
	pg := parsePagination(req)

	if pg.PerPage != 200 {
		t.Errorf("expected per_page capped at 200, got %d", pg.PerPage)
	}
}

func TestParsePagination_InvalidValues(t *testing.T) {
	req := httptest.NewRequest("GET", "/?page=abc&per_page=-1", nil)
	pg := parsePagination(req)

	if pg.Page != 1 {
		t.Errorf("expected page 1 for invalid input, got %d", pg.Page)
	}
	if pg.PerPage != 50 {
		t.Errorf("expected per_page 50 for invalid input, got %d", pg.PerPage)
	}
}
