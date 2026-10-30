package evidence

import "testing"

func TestIsImageAndIsVideo_ClassifyByKindOrTarget(t *testing.T) {
	cases := []struct {
		kind, target string
		image, video bool
	}{
		{kind: "screenshot", image: true},
		{kind: "Screenshot", image: true},
		{kind: "gif", image: true},
		{kind: "image", image: true},
		{kind: "video", video: true},
		{kind: "recording", video: true},
		{target: "shots/checkout.png", image: true},
		{target: "shots/CHECKOUT.JPG", image: true},
		{target: "a.jpeg", image: true},
		{target: "a.gif", image: true},
		{target: "a.webp", image: true},
		{target: "a.svg", image: true},
		{target: "a.avif", image: true},
		{target: "a.bmp", image: true},
		{target: "https://example.com/shot.png?raw=true", image: true},
		{target: "https://example.com/shot.png#top", image: true},
		{target: "a.mp4", video: true},
		{target: "a.webm", video: true},
		{target: "a.mov", video: true},
		{target: "https://example.com/clip.mp4?t=3", video: true},
		{kind: "log", target: "logs/cli-run.txt"},
		{kind: "command-output", target: "https://example.com/build.log"},
		{target: ""},
	}
	for _, tc := range cases {
		if got := IsImage(tc.kind, tc.target); got != tc.image {
			t.Errorf("IsImage(%q, %q) = %v, want %v", tc.kind, tc.target, got, tc.image)
		}
		if got := IsVideo(tc.kind, tc.target); got != tc.video {
			t.Errorf("IsVideo(%q, %q) = %v, want %v", tc.kind, tc.target, got, tc.video)
		}
		if got := IsMedia(tc.kind, tc.target); got != (tc.image || tc.video) {
			t.Errorf("IsMedia(%q, %q) = %v, want %v", tc.kind, tc.target, got, tc.image || tc.video)
		}
	}
}
