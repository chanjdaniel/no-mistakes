package evidence

import "strings"

// Image and video evidence never leaves the machine: the PR body describes it
// by its caption only and the evidence branch skips it. This file is the one
// owner of what counts as an image or a video, so the PR renderer and the
// publisher cannot disagree about it.

var (
	imageKinds      = map[string]bool{"screenshot": true, "gif": true, "image": true}
	videoKinds      = map[string]bool{"video": true, "recording": true}
	imageExtensions = []string{".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".avif", ".apng", ".bmp", ".tif", ".tiff", ".ico", ".heic"}
	videoExtensions = []string{".mp4", ".webm", ".mov", ".m4v", ".mkv", ".avi", ".ogv", ".mpg", ".mpeg"}
)

// IsImage reports whether an artifact of kind, or the file or URL target, is
// an image. The kind is an agent-reported label such as "screenshot"; target
// is a path or URL classified by its extension, with and without any query or
// fragment.
func IsImage(kind, target string) bool {
	return imageKinds[strings.ToLower(strings.TrimSpace(kind))] || hasExtension(target, imageExtensions)
}

// IsVideo is IsImage for videos and recordings.
func IsVideo(kind, target string) bool {
	return videoKinds[strings.ToLower(strings.TrimSpace(kind))] || hasExtension(target, videoExtensions)
}

// IsMedia reports whether kind or target is an image or a video.
func IsMedia(kind, target string) bool {
	return IsImage(kind, target) || IsVideo(kind, target)
}

func hasExtension(target string, extensions []string) bool {
	lower := strings.ToLower(strings.TrimSpace(target))
	if lower == "" {
		return false
	}
	candidates := []string{lower}
	if i := strings.IndexAny(lower, "?#"); i >= 0 {
		candidates = append(candidates, lower[:i])
	}
	for _, candidate := range candidates {
		for _, ext := range extensions {
			if strings.HasSuffix(candidate, ext) {
				return true
			}
		}
	}
	return false
}
