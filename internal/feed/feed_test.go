package feed

import (
	"testing"
)

const sample = `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns:yt="http://www.youtube.com/xml/schemas/2015" xmlns:media="http://search.yahoo.com/mrss/" xmlns="http://www.w3.org/2005/Atom">
 <title>Theo - t3․gg</title>
 <entry>
  <id>yt:video:oPZLPUtmROo</id>
  <yt:videoId>oPZLPUtmROo</yt:videoId>
  <title>Did AI kill React Native?</title>
  <published>2026-09-14T13:01:43+00:00</published>
 </entry>
 <entry>
  <id>yt:video:abcdefghijk</id>
  <yt:videoId>abcdefghijk</yt:videoId>
  <title>Older video</title>
  <published>2026-09-13T10:00:00+00:00</published>
 </entry>
</feed>`

func TestParseAtom(t *testing.T) {
	videos, err := parseAtom([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if len(videos) != 2 {
		t.Fatalf("want 2 videos, got %d", len(videos))
	}
	if videos[0].ID != "abcdefghijk" || videos[1].ID != "oPZLPUtmROo" {
		t.Fatalf("videos not sorted oldest first: %+v", videos)
	}
	if videos[1].Title != "Did AI kill React Native?" {
		t.Fatalf("bad title %q", videos[1].Title)
	}
	if videos[1].Published.IsZero() {
		t.Fatal("published not parsed")
	}
}

func TestParseAtomEmpty(t *testing.T) {
	if _, err := parseAtom([]byte(`<feed xmlns="http://www.w3.org/2005/Atom"></feed>`)); err == nil {
		t.Fatal("expected error for empty feed")
	}
}
