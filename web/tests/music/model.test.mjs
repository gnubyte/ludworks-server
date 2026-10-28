// Music app pure helpers (apps/music/model.js): routes and paths, art URLs, labels, sorts,
// permissions and error wording.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  isId, TABS, parseRoute, paths, artUrl, baseName, trackTitle, trackArtist, albumTitle, trackLabel, trackDuration, totalDuration,
  formatLongDuration, songCount, groupByDisc, codecLabel, audioQuality, channelLabel, replayGainLabel, albumSubtitle,
  SONG_SORTS, ALBUM_SORTS, ARTIST_SORTS, PLAYLIST_SORTS, normalizeSort, hasScope, canWriteSpace, canEditTracks, canEditAlbumArt,
  isPlaylistOwner, canEditItems, errorText, humanField, isVersionConflict, splitList, parseClock, chunk, randomSeed, playContext,
  recentLink, addedByLabel, trackNumberLabel,
} from '../../assets/js/apps/music/model.js';
import { iconNames } from '../../assets/js/core/icons.js';

const ID = 'abcdefghijklmnopqrstuvwxyz';

test('routes', () => {
  assert.deepEqual(parseRoute(''), { view: 'home', tab: 'home' });
  for (const v of ['songs', 'albums', 'artists', 'genres', 'playlists', 'favorites', 'history']) {
    assert.deepEqual(parseRoute(v), { view: v, tab: v });
    assert.deepEqual(parseRoute(`${v}/`), { view: v, tab: v });
  }
  assert.deepEqual(parseRoute(`albums/${ID}`), { view: 'album', id: ID, tab: 'albums' });
  assert.deepEqual(parseRoute(`artists/${ID}`), { view: 'artist', id: ID, tab: 'artists' });
  assert.deepEqual(parseRoute(`playlists/${ID}`), { view: 'playlist', id: ID, tab: 'playlists' });
  assert.deepEqual(parseRoute('genre'), { view: 'genre', tab: 'genres' });
  assert.deepEqual(parseRoute('search'), { view: 'search', tab: '' });
  assert.deepEqual(parseRoute('now-playing'), { view: 'nowplaying', tab: '' });
  for (const bad of ['albums/short', 'albums/a b c d e f', `albums/${ID}/x`, 'songs/x', 'nope', 'genre/rock', `history/${ID}`, '../etc']) {
    assert.equal(parseRoute(bad).view, 'notfound', bad);
  }
  assert.ok(isId(ID) && !isId('short') && !isId(7));
  for (const t of TABS) assert.ok(iconNames.includes(t.icon), `icon ${t.icon}`);
  assert.equal(paths.genre('Rock/Pop & Soul'), '/music/genre?name=Rock%2FPop%20%26%20Soul');
  assert.equal(paths.search('a b'), '/music/search?q=a%20b');
  assert.equal(paths.search(''), '/music/search');
  assert.equal(paths.nowPlaying('lyrics'), '/music/now-playing?tab=lyrics');
  assert.equal(paths.album(ID), `/music/albums/${ID}`);
  assert.equal(paths.favorites('albums'), '/music/favorites?tab=albums');
});

test('art URLs keep the cache-busting version', () => {
  assert.equal(artUrl('/api/v1/music/albums/x/art?v=abc', 'lg'), '/api/v1/music/albums/x/art?v=abc&size=lg');
  assert.equal(artUrl('/api/v1/music/albums/x/art', 'sm'), '/api/v1/music/albums/x/art?size=sm');
  assert.equal(artUrl('/api/v1/music/albums/x/art?v=1', 'huge'), '/api/v1/music/albums/x/art?v=1&size=md');
  assert.equal(artUrl(null), null);
  assert.equal(artUrl('https://evil.example/x.jpg'), null, 'only API paths');
  assert.equal(artUrl('javascript:alert(1)'), null);
});

test('labels', () => {
  assert.equal(baseName('03 - Song.flac'), '03 - Song');
  assert.equal(baseName('.hidden'), '.hidden');
  assert.equal(trackTitle({ title: ' ', name: 'x.mp3' }), 'x');
  assert.equal(trackTitle({}), 'Untitled');
  assert.equal(trackArtist({ artist: '', artists: ['A', 'B'] }), 'A, B');
  assert.equal(trackArtist({}), 'Unknown artist');
  assert.equal(albumTitle(''), 'Unknown album');
  assert.equal(albumTitle({ title: 'X' }), 'X');
  assert.equal(trackLabel({ title: 'T', artist: 'A' }), 'T — A');
  assert.equal(trackDuration({ duration_seconds: null }), 0);
  assert.equal(totalDuration([{ duration_seconds: 60 }, { duration_seconds: null }, { duration_seconds: 30.5 }]), 90.5);
  assert.equal(formatLongDuration(0), '0 min');
  assert.equal(formatLongDuration(35), '35 sec');
  assert.equal(formatLongDuration(42 * 60 + 10), '42 min');
  assert.equal(formatLongDuration(3600 + 5 * 60), '1 hr 5 min');
  assert.equal(formatLongDuration(2 * 3600 + 59 * 60 + 50), '3 hr');
  assert.equal(formatLongDuration(7200), '2 hr');
  assert.equal(songCount(1), '1 song');
  assert.equal(songCount(2), '2 songs');
  assert.equal(trackNumberLabel({ track_number: 3 }), '3');
  assert.equal(trackNumberLabel({ track_number: null }), '');
  assert.equal(codecLabel('flac'), 'FLAC');
  assert.equal(codecLabel('wav_0x0161'), 'WAV_0X0161');
  assert.equal(audioQuality({ codec: 'flac', sample_rate: 44100, bit_depth: 16 }), 'FLAC · 44.1 kHz · 16-bit');
  assert.equal(audioQuality({ codec: 'pcm', sample_rate: 22050, bit_depth: 16 }), 'PCM · 22.05 kHz · 16-bit');
  assert.equal(audioQuality({ codec: 'alac', sample_rate: 96000, bit_depth: 24 }), 'ALAC · 96 kHz · 24-bit');
  assert.equal(audioQuality({ codec: 'mp3', bitrate_kbps: 320 }), 'MP3 · 320 kbps');
  assert.equal(audioQuality({}), '');
  assert.equal(channelLabel(2), 'Stereo');
  assert.equal(channelLabel(1), 'Mono');
  assert.equal(channelLabel(6), '5.1');
  assert.equal(channelLabel(3), '3 channels');
  assert.equal(channelLabel(null), '');
  assert.equal(replayGainLabel({ track_gain_db: -6.5, track_peak: 0.98 }), '−6.50 dB (peak 0.980)');
  assert.equal(replayGainLabel({ track_gain_db: 2, track_peak: 1 }), '+2.00 dB');
  assert.equal(replayGainLabel({ track_gain_db: -1, track_peak: 1, album_gain_db: null }, 'album'), '');
  assert.equal(replayGainLabel(null), '');
  assert.equal(albumSubtitle({ artist: 'A', year: 1999 }), 'A · 1999');
  assert.equal(albumSubtitle({}), 'Unknown artist');
});

test('disc groups', () => {
  const one = [{ disc_number: 1 }, { disc_number: null }];
  assert.deepEqual(groupByDisc(one), [{ disc: null, tracks: one }]);
  const two = [{ id: 'a', disc_number: 1 }, { id: 'b', disc_number: 2 }, { id: 'c', disc_number: 2 }];
  assert.deepEqual(groupByDisc(two).map((g) => [g.disc, g.tracks.map((t) => t.id)]), [[1, ['a']], [2, ['b', 'c']]]);
  assert.deepEqual(groupByDisc([]), [{ disc: null, tracks: [] }]);
});

test('sorts', () => {
  for (const list of [SONG_SORTS, ALBUM_SORTS, ARTIST_SORTS, PLAYLIST_SORTS]) {
    for (const o of list) assert.ok(o.order === 'asc' || o.order === 'desc');
  }
  assert.deepEqual(normalizeSort(SONG_SORTS, 'added', null), { sort: 'added', order: 'desc' });
  assert.deepEqual(normalizeSort(SONG_SORTS, 'added', 'asc'), { sort: 'added', order: 'asc' });
  assert.deepEqual(normalizeSort(SONG_SORTS, 'nope', 'up'), { sort: 'title', order: 'asc' });
  assert.deepEqual(SONG_SORTS.map((s) => s.value), ['title', 'artist', 'album', 'added', 'year', 'duration', 'plays', 'last_played']);
});

test('permissions', () => {
  const spaces = [{ id: 'home0001', access: 'owner' }, { id: 'libw0001', access: 'write' }, { id: 'libr0001', access: 'read' }];
  assert.ok(hasScope(['*'], 'music:write'));
  assert.ok(hasScope(['music:read', 'music:write'], 'music:write'));
  assert.ok(!hasScope(['music:read'], 'music:write'));
  assert.ok(!hasScope(null, 'music:read'));
  assert.ok(canWriteSpace(spaces, 'home0001'));
  assert.ok(canWriteSpace(spaces, 'libw0001'));
  assert.ok(!canWriteSpace(spaces, 'libr0001'));
  assert.ok(!canWriteSpace(spaces, 'unknown1'));
  assert.ok(canWriteSpace(null, 'unknown1', true), 'admins write every space');
  const who = { scopes: ['*'], spaces, isAdmin: false };
  assert.ok(canEditTracks([{ space_id: 'home0001' }, { space_id: 'libw0001' }], who));
  assert.ok(!canEditTracks([{ space_id: 'home0001' }, { space_id: 'libr0001' }], who), 'every track must be writable');
  assert.ok(!canEditTracks([], who));
  assert.ok(!canEditTracks([{ space_id: 'home0001' }], { ...who, scopes: ['music:read'] }));
  assert.ok(canEditAlbumArt([{ space_id: 'libw0001' }], who));
  assert.ok(isPlaylistOwner({ owner_id: 'u1' }, 'u1'));
  assert.ok(!isPlaylistOwner({ owner_id: 'u1' }, 'u2'));
  assert.ok(!isPlaylistOwner(null, 'u1'));
  assert.ok(canEditItems({ kind: 'manual', can_edit: true }));
  assert.ok(!canEditItems({ kind: 'smart', can_edit: true }));
  assert.ok(!canEditItems({ kind: 'manual', can_edit: false }));
});

test('error wording', () => {
  assert.match(errorText({ status: 501, code: 'ffmpeg_unavailable', message: 'x' }), /ffmpeg/);
  assert.match(errorText({ status: 429, retryAfter: 7 }), /Try again in 7 s/);
  assert.match(errorText({ status: 429 }), /in a moment/);
  assert.match(errorText({ status: 409, details: { current: {} } }), /changed this playlist/);
  assert.equal(errorText({ status: 409, message: 'You have reached the limit of 1000 playlists' }), 'You have reached the limit of 1000 playlists');
  assert.equal(errorText({ status: 422, details: { fields: { 'rules.conditions[1].value': 'must be a number' } } }), 'Condition 2 value: must be a number');
  assert.equal(errorText({ status: 422, message: 'M3U files hold at most 20000 entries' }), 'M3U files hold at most 20000 entries');
  assert.match(errorText({ status: 415 }), /JPEG/);
  assert.match(errorText({ status: 413 }), /too large/);
  assert.equal(errorText(null, 'fb'), 'fb');
  assert.equal(errorText('plain'), 'plain');
  assert.equal(humanField('track_ids'), 'Track ids');
  assert.equal(humanField('set.album_artist'), 'Album artist');
  assert.ok(isVersionConflict({ status: 409, details: { current: { id: 'x' } } }));
  assert.ok(!isVersionConflict({ status: 409, details: null }));
});

test('utilities', () => {
  assert.deepEqual(splitList('Rock; Pop ;; Rock'), ['Rock', 'Pop']);
  assert.deepEqual(splitList('A, B; C', { commas: true }), ['A', 'B', 'C']);
  assert.deepEqual(splitList('Earth, Wind & Fire'), ['Earth, Wind & Fire'], 'commas only split when asked');
  assert.equal(parseClock('3:30'), 210);
  assert.equal(parseClock('1:02:03'), 3723);
  assert.equal(parseClock('95'), 95);
  assert.equal(parseClock('2:75'), null);
  assert.equal(parseClock('1:61:00'), null);
  assert.equal(parseClock(''), null);
  assert.equal(parseClock('abc'), null);
  assert.deepEqual(chunk([1, 2, 3, 4, 5], 2), [[1, 2], [3, 4], [5]]);
  assert.match(randomSeed(() => 0.5), /^[0-9a-z]+$/);
  assert.deepEqual(playContext('album', 'abcdefgh'), { kind: 'album', id: 'abcdefgh' });
  assert.deepEqual(playContext('queue'), { kind: 'queue' });
  assert.equal(playContext('radio', 'x'), null);
  assert.equal(recentLink({ kind: 'album', album: { id: ID } }), `/music/albums/${ID}`);
  assert.equal(recentLink({ kind: 'playlist', playlist: { id: ID } }), `/music/playlists/${ID}`);
  assert.equal(recentLink({ kind: 'track', track: { album_id: '' } }), null);
  assert.equal(recentLink(null), null);
  const pl = { owner_id: 'owner', owner_name: 'Olga' };
  assert.equal(addedByLabel({ added_by: 'me' }, pl, 'me'), 'you');
  assert.equal(addedByLabel({ added_by: 'owner' }, pl, 'me'), 'Olga');
  assert.equal(addedByLabel({ added_by: 'other' }, pl, 'me'), 'another user');
  assert.equal(addedByLabel({ added_by: '' }, pl, 'me'), '', 'deleted users');
});
