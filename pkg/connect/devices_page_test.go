package connect

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// The fixture is a reduced copy of the real devices page, so a change in how
// the page is parsed is checked against the markup the service serves.
func TestParseDevicesPageFixture(t *testing.T) {
	page, err := os.ReadFile("testdata/devices_page.html")
	if err != nil {
		t.Fatal(err)
	}

	got, err := parseDevicesPage(string(page))
	if err != nil {
		t.Fatalf("parseDevicesPage failed: %v", err)
	}

	want := []DeviceInfo{
		// The name is HTML-escaped on the page; it has to match what the user types.
		{ID: "11111111-aaaa-4bbb-8ccc-000000000001", Name: "Tom's Pi & <lab>", Online: false, Shell: false, Version: "2.14.0"},
		// The row's icons contain digits that look like a version ("011.06.02").
		{ID: "22222222-aaaa-4bbb-8ccc-000000000002", Name: "kitchen-pi", Online: true, Shell: true, Version: "2.13.1"},
		{ID: "33333333-aaaa-4bbb-8ccc-000000000003", Name: "u automatů (hall)", Online: true, Shell: false, Version: "2.14.0"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("devices parsed from the fixture:\n got: %+v\nwant: %+v", got, want)
	}
}

func TestParseDevicesPageNameWithNestedMarkup(t *testing.T) {
	page := `<table><tr><td><a class="link" href="/devices/abc-123"> <span class="x">garage</span>
		pi </a></td><td>2.1.0</td><td><div>Online</div></td></tr></table>`
	got, err := parseDevicesPage(page)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "garage pi" {
		t.Fatalf("expected one device named %q, got %+v", "garage pi", got)
	}
}

func TestParseDevicesPageEmptyAccount(t *testing.T) {
	got, err := parseDevicesPage(`<html><body><main><p>You have no devices yet.</p></main></body></html>`)
	if err != nil {
		t.Fatalf("an account without devices is not an error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no devices, got %+v", got)
	}
}

// If the page lists devices in markup this parser does not understand, say
// so. An empty list would look like an account without devices.
func TestParseDevicesPageUnrecognisedMarkup(t *testing.T) {
	page := `<html><body><ul>
		<li><a href="/devices/11111111-aaaa-4bbb-8ccc-000000000001">kitchen-pi</a> Online</li>
	</ul></body></html>`
	got, err := parseDevicesPage(page)
	if err == nil {
		t.Fatalf("expected an error for unrecognised device markup, got %+v", got)
	}
	if !strings.Contains(err.Error(), "markup") {
		t.Errorf("error should say the page markup was not recognised, got: %v", err)
	}
}
