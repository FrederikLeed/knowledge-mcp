package htmlmarkdown

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func TestConvert(t *testing.T) {
	t.Parallel()
	source := []byte("<h1>Title</h1><p>&nbsp;</p><p>   </p><p>Text with <a href=\"/x\">a [link]</a> and <b>bold</b>.<br>Next line</p>" +
		"<p>Before table</p><table><tr><th>A</th><th>B</th></tr><tr><td>1|2</td><td>3<br>4<p>5</p></td></tr></table><ul><li>one</li><li>two<ul><li>nested</li></ul></li></ul>" +
		"<script>ignored()</script><div class=\"ad\">skipped</div>")
	got := Convert(source, Options{
		Link: func(href string) string { return "https://example.test" + href },
		Skip: func(node *html.Node) bool { return Attribute(node, "class") == "ad" },
	})
	want := "# Title\n\nText with [a \\[link\\]](https://example.test/x) and **bold**.  \nNext line\n\nBefore table\n\n| A | B | \n| --- | --- | \n| 1\\|2 | 3 4 5 | \n\n- one\n- two\n  - nested"
	if got != want {
		t.Fatalf("Convert =\n%q\nwant\n%q", got, want)
	}
	if strings.Contains(got, "ignored") || strings.Contains(got, "skipped") {
		t.Fatal("script or skipped node rendered")
	}
}
