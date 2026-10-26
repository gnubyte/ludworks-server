-- Feed tokens no longer carry media:read (API.md §7.2): every feed, program, live.ts and
-- HDHomeRun route needs only tv:read, and a leaked feed URL must not expose the media library
-- outside the user's channels. Existing feed tokens are narrowed in place, so URLs already
-- pasted into IPTV apps, Plex or Jellyfin keep working. (The tv_feed_tokens comment in 0001
-- predates this change.)
UPDATE personal_access_tokens SET scopes = '["tv:read"]'
WHERE id IN (SELECT token_id FROM tv_feed_tokens);
