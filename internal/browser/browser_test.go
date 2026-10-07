package browser

import "testing"

// The block list is a small win, and it used to be a large and wrong one: it
// dropped branddb.*, imageEditor/* and pdf-all, which are exactly the bundles
// that build the search form. That did not fail loudly — it produced a page that
// looked loaded, had no #AUTO_input, no mode links and no jQuery.fn.solrManager,
// and silently collected nothing. These tests therefore pin both halves hard:
// what may be dropped, and what must never be.
func TestShouldBlockURLDropsChrome(t *testing.T) {
	blocked := []struct {
		url  string
		kind string
	}{
		// Decoration, by resource type: the biggest per-page cost on a proxied run.
		{"https://www3.wipo.int/madrid/monitor/images/active.png", "image"},
		{"https://www3.wipo.int/madrid/monitor/images/ajax-busy-fafafa-405176.gif", "image"},
		{"https://www3.wipo.int/export/system/modules/webfonts/ss-standard.woff2", "font"},
		{"https://www3.wipo.int/fonts/files/noto-sans-display-latin-400-normal.woff2", "font"},

		// Third-party tracking.
		{"https://www.googletagmanager.com/gtm.js?id=GTM-P7RLS2", "script"},
		{"https://www.google-analytics.com/analytics.js", "script"},
		{"https://wipoanalytics.wipo.int/matomo/js/container_jdTOcj6j.js", "script"},

		// The IP-portal navbar, which is chrome around the monitor and not part of it.
		{"https://webcomponents.wipo.int/wipo-navbar/wipo-navbar.js", "script"},
		{"https://webcomponents.wipo.int/wipo-init/wipo-init.js", "script"},
		{"https://webcomponents.wipo.int/polyfills/webcomponents-loader.js", "script"},
		{"https://webcomponents.wipo.int/wipo-navbar/ulf-fonts.css", "stylesheet"},
	}
	for _, tt := range blocked {
		if !shouldBlockURL(tt.url, tt.kind) {
			t.Errorf("shouldBlockURL(%q, %q) = false, want true", tt.url, tt.kind)
		}
	}
}

func TestShouldBlockURLKeepsTheSearchAndResults(t *testing.T) {
	// Everything here either builds the search form or carries the results.
	// Blocking any of it does not make the collector faster, it makes it blind.
	keep := []struct {
		url  string
		kind string
	}{
		{"https://www3.wipo.int/madrid/monitor/en/", "document"},
		{"https://www3.wipo.int/madrid/monitor/jsp/select.jsp", "document"},
		{"https://www3.wipo.int/madrid/monitor/en/showData.jsp?ID=BAN:AM%202018/2004", "document"},
		{"https://www3.wipo.int/madrid/monitor/data/branddb_view.jsp?id=1", "document"},

		// The bundles that create #AUTO_input. This is the regression that
		// motivated the list in the first place.
		{"https://www3.wipo.int/madrid/monitor/branddb.init.16117.min.js", "script"},
		{"https://www3.wipo.int/madrid/monitor/scripts/branddb.en.16117.min.js", "script"},
		{"https://www3.wipo.int/madrid/monitor/scripts/branddb-required.16117.min.js", "script"},
		{"https://www3.wipo.int/madrid/monitor/scripts/imageEditor/runtime.js", "script"},
		{"https://www3.wipo.int/madrid/monitor/scripts/imageEditor/main.js", "script"},
		{"https://www3.wipo.int/madrid/monitor/scripts/pdf-all.16117.min.js", "script"},
		{"https://www3.wipo.int/madrid/monitor/scripts/swfobject.min.js", "script"},

		// The site's own runtime.
		{"https://www3.wipo.int/madrid/monitor/scripts/source/romarin.emadrid.v15.new.16117.min.js", "script"},
		{"https://www3.wipo.int/madrid/monitor/scripts/source/jquery/jquery-1.12.2.js", "script"},
		{"https://www3.wipo.int/madrid/monitor/scripts/source/jquery/jquery-ui-1.11.4.min.js", "script"},
		{"https://www3.wipo.int/madrid/monitor/scripts/tmpl.js", "script"},
		{"https://www3.wipo.int/madrid/monitor/scripts/plugins.js", "script"},
		{"https://www3.wipo.int/madrid/monitor/css/monitor.css", "stylesheet"},
		{"https://www3.wipo.int/madrid/monitor/css/branddb.16023.min.css", "stylesheet"},

		// The search itself: the suggestion lookup, the results page and the
		// per-record logo.
		{"https://www3.wipo.int/madrid/monitor/jsp/terms.jsp?&terms=true&terms.sort=count", "xhr"},
		{"https://www3.wipo.int/madrid/monitor/jsp/select.jsp", "xhr"},
		{"https://www3.wipo.int/madrid/monitor/jsp/data.jsp?KEY=ROM.1069631&TYPE=jpg", "fetch"},
	}
	for _, tt := range keep {
		if shouldBlockURL(tt.url, tt.kind) {
			t.Errorf("shouldBlockURL(%q, %q) = true — this builds the search or carries the results", tt.url, tt.kind)
		}
	}
}
