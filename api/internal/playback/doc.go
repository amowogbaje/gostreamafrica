// Package playback authorizes viewing and hands out short-lived signed media URLs.
//
// The API never serves video. It checks that the viewer may watch the title (the Entitlements hook,
// which Phase 4 replaces with real access rules) and returns a manifest URL whose path carries an
// expiring token. The media front door (nginx locally, Cloudflare or Bunny in the cloud) checks the
// token on every request, manifests and segments alike. See docs/playback.md.
package playback
