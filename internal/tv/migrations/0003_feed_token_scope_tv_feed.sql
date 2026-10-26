-- Feed tokens carry the dedicated scope tv:feed instead of tv:read (API.md §7.2): tv:feed
-- reaches only the M3U/XMLTV feeds, channel logos, live.ts, play and program streams and the
-- HDHomeRun emulation, not the rest of the TV API (lineups, channels, guide, positions,
-- posters, subtitles, transcodes). Existing feed tokens that carry exactly tv:read are narrowed
-- in place, so URLs already pasted into IPTV apps, Plex or Jellyfin keep working.
UPDATE personal_access_tokens SET scopes = '["tv:feed"]'
WHERE id IN (SELECT token_id FROM tv_feed_tokens) AND scopes = '["tv:read"]';
