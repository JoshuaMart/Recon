package search

import "testing"

func TestExportURL(t *testing.T) {
	host := "app.example.com"
	ipv6 := "2001:db8::1"
	https := "https"
	http := "http"
	ssh := "ssh"
	port443 := int32(443)
	port8443 := int32(8443)
	port8080 := int32(8080)

	tests := []struct {
		name string
		row  Row
		want string
	}{
		{
			name: "declared URL keeps its path",
			row:  Row{Kind: "url", Key: "https://app.example.com/login"},
			want: "https://app.example.com/login",
		},
		{
			name: "default port is omitted",
			row:  Row{Kind: "service", Host: &host, Port: &port443, Scheme: &https},
			want: "https://app.example.com",
		},
		{
			name: "non-default port is kept",
			row:  Row{Kind: "service", Host: &host, Port: &port8080, Scheme: &http},
			want: "http://app.example.com:8080",
		},
		{
			name: "IPv6 authority is bracketed",
			row:  Row{Kind: "service", Host: &ipv6, Port: &port8443, Scheme: &https},
			want: "https://[2001:db8::1]:8443",
		},
		{name: "unmeasured service is omitted", row: Row{Kind: "service", Host: &host, Port: &port443}},
		{name: "non-web scheme is omitted", row: Row{Kind: "service", Host: &host, Port: &port443, Scheme: &ssh}},
		{name: "non-web asset is omitted", row: Row{Kind: "fqdn", Key: host}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := exportURL(tt.row); got != tt.want {
				t.Errorf("exportURL() = %q, want %q", got, tt.want)
			}
		})
	}
}
