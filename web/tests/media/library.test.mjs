// TV library pure helpers: routes, browse state → API query, labels, editing, shows and
// lineup items.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  parseRoute, libraryHref, itemHref, showHref, playHref, toQueryString, browseState, listQuery, activeFilterCount,
  episodeCode, resolutionLabel, runtime, remainingLabel, progressFraction, canResume, displayTitle, cardSubtitle, sizedUrl,
  thumbUrl, placeholderArt, techBadges, parseGenres, normalizeGenres, intField, validateEdit, diffMedia, nextEpisode,
  seasonLabel, decades, fileLineupItem, showLineupItem, seasonLineupItem, folderLineupItem, queryLineupItem, VIEWS, PRESETS,
  subtitleFormat, defaultOrder, collectPicks,
} from '../../assets/js/apps/tv/library/model.js';
import { iconNames } from '../../assets/js/core/icons.js';

test('routes', () => {
  assert.deepEqual(parseRoute(''), { view: 'home' });
  assert.deepEqual(parseRoute('/'), { view: 'home' });
  assert.deepEqual(parseRoute('movies'), { view: 'browse', preset: 'movies' });
  assert.deepEqual(parseRoute('search'), { view: 'browse', preset: 'search' });
  assert.deepEqual(parseRoute('browse'), { view: 'browse', preset: 'browse' });
  assert.deepEqual(parseRoute('shows'), { view: 'shows' });
  assert.deepEqual(parseRoute('shows/the-office'), { view: 'show', key: 'the-office' });
  assert.deepEqual(parseRoute('shows/caf%C3%A9'), { view: 'show', key: 'café' });
  assert.deepEqual(parseRoute('item/abc123def'), { view: 'item', id: 'abc123def' });
  assert.deepEqual(parseRoute('item/%E0%A4%A'), { view: 'item', id: '%E0%A4%A' }); // malformed escapes don't throw
  assert.deepEqual(parseRoute('genres'), { view: 'genres' });
  assert.deepEqual(parseRoute('years'), { view: 'years' });
  assert.deepEqual(parseRoute('nope'), { view: 'notfound' });
  assert.deepEqual(parseRoute('constructor'), { view: 'notfound' });
  assert.equal(libraryHref(), '/tv/library');
  assert.equal(libraryHref('movies', { genre: 'Sci-Fi', year: 2001, x: '' }), '/tv/library/movies?genre=Sci-Fi&year=2001');
  assert.equal(itemHref('a/b'), '/tv/library/item/a%2Fb');
  assert.equal(showHref('the-office'), '/tv/library/shows/the-office');
  assert.equal(playHref('id1'), '/tv/play/id1');
  assert.equal(playHref('id1', 61.9), '/tv/play/id1?t=61');
  assert.equal(playHref('id1', -5), '/tv/play/id1?t=0');
  assert.equal(toQueryString({ b: 2, a: 'x y' }), '?a=x+y&b=2');
  assert.equal(toQueryString({}), '');
  for (const v of VIEWS) assert.ok(iconNames.includes(v.icon), v.icon);
  for (const v of VIEWS.filter((x) => !['home', 'shows', 'genres', 'years'].includes(x.id))) assert.ok(v.id in PRESETS, v.id);
});

test('browse state and list query', () => {
  let s = browseState({}, 'movies');
  assert.deepEqual(s, { filters: {}, sort: 'title', order: 'asc', seed: '' });
  assert.deepEqual(listQuery('movies', s, 60), { category: 'movie', sort: 'title', order: 'asc', limit: 60 });
  s = browseState({ genre: ' Drama ', year: '1999', watched: 'maybe', category: 'tv', sort: 'year' }, 'movies');
  // The preset's fixed category cannot be overridden; invalid values are dropped; sort picks its default order.
  assert.deepEqual(s.filters, { genre: 'Drama', year: '1999' });
  assert.equal(s.sort, 'year');
  assert.equal(s.order, 'desc');
  assert.deepEqual(listQuery('movies', s), { category: 'movie', genre: 'Drama', year: '1999', sort: 'year', order: 'desc', limit: 60 });
  s = browseState({ year: '19x9', sort: 'bogus', order: 'sideways' }, 'recent');
  assert.deepEqual(s, { filters: {}, sort: 'added', order: 'desc', seed: '' });
  s = browseState({ sort: 'random', seed: 'abc123' }, 'browse');
  assert.equal(listQuery('browse', s).seed, 'abc123');
  s = browseState({ sort: 'random', seed: '<script>' }, 'browse');
  assert.equal(s.seed, '');
  assert.equal(listQuery('unwatched', browseState({ watched: 'true' }, 'unwatched')).watched, 'false');
  assert.equal(listQuery('videos', browseState({}, 'videos')).category, 'none');
  assert.equal(activeFilterCount({ q: 'x', genre: 'a', year: '1' }), 2);
  assert.equal(defaultOrder('added'), 'desc');
  assert.equal(defaultOrder('nope'), 'asc');
});

test('labels', () => {
  assert.equal(episodeCode(1, 2), 'S01E02');
  assert.equal(episodeCode(10, 112), 'S10E112');
  assert.equal(episodeCode(null, 3), 'E03');
  assert.equal(episodeCode(2, null), 'S02');
  assert.equal(episodeCode(null, null), '');
  assert.equal(resolutionLabel(3840, 2160), '4K');
  assert.equal(resolutionLabel(1920, 800), '1080p'); // scope
  assert.equal(resolutionLabel(1440, 1080), '1080p');
  assert.equal(resolutionLabel(1280, 720), '720p');
  assert.equal(resolutionLabel(1080, 1920), '1080p'); // portrait
  assert.equal(resolutionLabel(720, 480), '480p');
  assert.equal(resolutionLabel(320, 240), 'SD');
  assert.equal(resolutionLabel(null, 720), '');
  assert.equal(runtime(5760), '1h 36m');
  assert.equal(runtime(3600), '1h');
  assert.equal(runtime(2700), '45m');
  assert.equal(runtime(25), '25s');
  assert.equal(runtime(null), '');
  assert.equal(runtime(-1), '');
  const m = { duration_seconds: 6000, position_seconds: 1500, watched: false };
  assert.equal(progressFraction(m), 0.25);
  assert.equal(remainingLabel(m), '1h 15m left');
  assert.ok(canResume(m));
  assert.equal(progressFraction({ ...m, watched: true }), 0);
  assert.ok(!canResume({ ...m, position_seconds: 5 }));
  assert.ok(!canResume({ ...m, position_seconds: 5990 }));
  assert.equal(progressFraction({ position_seconds: 10 }), 0);
  assert.equal(displayTitle({ title: 'A', episode_title: 'B' }), 'A');
  assert.equal(displayTitle({ title: '', episode_title: 'B' }), 'B');
  assert.equal(cardSubtitle({ year: 1968, duration_seconds: 5760 }), '1968 · 1h 36m');
  assert.equal(cardSubtitle({ show_title: 'Show', season: 1, episode: 2 }), 'Show · S01E02');
  assert.equal(cardSubtitle({ show_title: 'Show', season: 1, episode: 2 }, { withShow: false }), 'S01E02');
  assert.equal(sizedUrl('/api/v1/media/x/poster', 'md'), '/api/v1/media/x/poster?size=md');
  assert.equal(sizedUrl('/api/v1/media/x/poster?size=sm&v=2', 'lg'), '/api/v1/media/x/poster?size=lg&v=2');
  assert.equal(sizedUrl(null, 'md'), '');
  assert.equal(thumbUrl({ id: 'f1', has_thumbnail: true }, 'lg'), '/api/v1/files/f1/thumbnail?size=lg');
  assert.equal(thumbUrl({ id: 'f1', has_thumbnail: false }), '');
  const a = placeholderArt('The Matrix Reloaded');
  assert.equal(a.initials, 'MR');
  assert.ok(a.hue >= 0 && a.hue < 360 && a.hue2 >= 0 && a.hue2 < 360);
  assert.deepEqual(placeholderArt('The Matrix Reloaded'), a); // stable
  assert.equal(placeholderArt('Alien').initials, 'AL');
  assert.equal(placeholderArt('').initials, '?');
  assert.equal(placeholderArt('ünïcode titel').initials, 'ÜT');
  assert.equal(placeholderArt('火垂るの墓').initials, '火垂');
  assert.deepEqual(techBadges({ width: 1920, height: 1080, video_codec: 'hevc', audio_codec: 'ac3', container: 'mkv' }), ['1080p', 'HEVC', 'AC3', 'MKV']);
  assert.equal(subtitleFormat('embedded'), 'Embedded');
  assert.equal(subtitleFormat('srt'), 'SRT');
});

test('editing helpers', () => {
  assert.deepEqual(parseGenres('Drama, sci-fi ,DRAMA,, Film  Noir'), ['Drama', 'sci-fi', 'Film Noir']);
  assert.deepEqual(normalizeGenres(['a'.repeat(80)]), ['a'.repeat(64)]);
  assert.equal(normalizeGenres(Array.from({ length: 60 }, (_, i) => `g${i}`)).length, 50);
  assert.equal(intField(''), null);
  assert.equal(intField(' 42 '), 42);
  assert.ok(Number.isNaN(intField('4.5')));
  assert.ok(Number.isNaN(intField('-1')));
  assert.deepEqual(validateEdit({ title: 'X', year: 1999, season: null, episode: 3, tmdb_id: null, imdb_id: 'tt0063350' }), {});
  const errs = validateEdit({ title: ' ', year: 1700, season: Number.NaN, tmdb_id: 0, imdb_id: 'nm1', content_rating: 'x'.repeat(40) });
  assert.deepEqual(Object.keys(errs).sort(), ['content_rating', 'imdb_id', 'season', 'title', 'tmdb_id', 'year']);
  const orig = { title: 'A', sort_title: 'a', year: null, genres: ['Drama'], description: '', season: 1, tmdb_id: null };
  assert.deepEqual(diffMedia(orig, { title: 'A', sort_title: 'a', year: null, genres: ['Drama'], description: '', season: 1, tmdb_id: null }), {});
  assert.deepEqual(diffMedia(orig, { title: 'B', year: 2001, genres: ['Drama', 'War'], season: null, imdb_id: '' }),
    { title: 'B', year: 2001, genres: ['Drama', 'War'], season: null });
});

test('shows and decades', () => {
  const ep = (id, season, episode, extra = {}) => ({ id, media: { season, episode, duration_seconds: 1000, position_seconds: 0, watched: false, ...extra } });
  const seasons = [
    { season: 1, episodes: [ep('a', 1, 1, { watched: true }), ep('b', 1, 2), ep('c', 1, 3, { position_seconds: 300 })] },
    { season: 2, episodes: [ep('d', 2, 1)] },
  ];
  assert.equal(nextEpisode(seasons).id, 'c'); // in progress wins
  seasons[0].episodes[2].media.position_seconds = 0;
  assert.equal(nextEpisode(seasons).id, 'b'); // first unwatched
  for (const s of seasons) for (const e of s.episodes) e.media.watched = true;
  assert.equal(nextEpisode(seasons).id, 'a'); // all watched: start over
  assert.equal(nextEpisode([]), null);
  assert.equal(seasonLabel(0), 'Specials');
  assert.equal(seasonLabel(3), 'Season 3');
  const d = decades([{ value: 1968, count: 2 }, { value: 2001, count: 1 }, { value: 1962, count: 3 }, { value: 'x', count: 9 }]);
  assert.deepEqual(d.map((x) => [x.decade, x.count, x.years.map((y) => y.value)]), [[2000, 1, [2001]], [1960, 5, [1968, 1962]]]);
});

test('lineup items', () => {
  const movie = { id: 'f1', name: 'x.mp4', media: { title: 'Alien', year: 1979 } };
  assert.deepEqual(fileLineupItem(movie), { kind: 'file', file_id: 'f1', order: 'default', label: 'Alien' });
  const episode = { id: 'f2', name: 'y.mkv', media: { title: 'Pilot', show_title: 'Show', season: 1, episode: 1, episode_title: 'Pilot' } };
  assert.deepEqual(fileLineupItem(episode), { kind: 'file', file_id: 'f2', order: 'default', label: 'Show S01E01 · Pilot' });
  assert.deepEqual(fileLineupItem({ id: 'f3', name: 'z.mkv', media: { title: '' } }).label, 'z.mkv');
  assert.deepEqual(showLineupItem({ show_title: 'Show' }, { order: 'random', limit: 10 }), { kind: 'show', show_title: 'Show', order: 'random', label: 'Show', limit: 10 });
  assert.deepEqual(seasonLineupItem('Show', 0), { kind: 'season', show_title: 'Show', season: 0, order: 'default', label: 'Show · Specials' });
  assert.deepEqual(folderLineupItem('sp1', 'TV/Cartoons', { recursive: false, limit: '0' }),
    { kind: 'folder', space_id: 'sp1', path: 'TV/Cartoons', recursive: false, order: 'default', label: 'Folder · Cartoons' });
  assert.equal(folderLineupItem('sp1', '', { spaceName: 'Movies' }).label, 'Folder · Movies');
  assert.deepEqual(queryLineupItem({ category: 'movie', genre: 'Horror', year: '1968', tag: '', bogus: 'x' }, { order: 'random', limit: 5 }),
    { kind: 'query', query: { category: 'movie', genre: 'Horror', year: 1968 }, order: 'random', label: 'Smart · Horror, category movie, 1968', limit: 5 });
  assert.equal(queryLineupItem({}).label, 'Smart · everything');
});

test('picker collects selections from every tab', () => {
  // Regression: picking a movie, switching to Shows and picking a show added only the show.
  const movie = fileLineupItem({ id: 'f1', name: 'x.mp4', media: { title: 'Alien', year: 1979 } });
  const picked = {
    files: new Map([['f1', movie]]),
    show: new Map([['show:Show', { show_title: 'Show' }]]),
    season: new Map([['season:Other:2', { show_title: 'Other', season: 2 }]]),
  };
  const items = collectPicks(picked, null, { order: 'random', limit: 3 });
  assert.deepEqual(items.map((i) => i.kind), ['file', 'show', 'season']);
  assert.deepEqual(items[0], movie, 'file items are passed through untouched');
  assert.deepEqual(items[1], { kind: 'show', show_title: 'Show', order: 'random', label: 'Show', limit: 3 });
  assert.deepEqual(items[2], { kind: 'season', show_title: 'Other', season: 2, order: 'random', label: 'Other · Season 2', limit: 3 });
  const extra = queryLineupItem({ genre: 'Drama' });
  assert.deepEqual(collectPicks(picked, extra).at(-1), extra, 'the active form tab is added last');
  assert.deepEqual(collectPicks({ files: new Map(), show: new Map(), season: new Map() }, null), []);
});

test('library modules link and export the TV contract', async () => {
  const lib = await import('../../assets/js/apps/tv/library/index.js');
  for (const fn of ['mountLibrary', 'openMetadataEditor', 'pickMedia']) assert.equal(typeof lib[fn], 'function', fn);
  assert.equal(lib.mountLibrary.length, 2); // (container, ctx, rest = '')
});

test('every icon the library uses exists', async () => {
  const { readdirSync, readFileSync, statSync } = await import('node:fs');
  const { join } = await import('node:path');
  const root = new URL('../../assets/js/apps/tv/library/', import.meta.url).pathname;
  const files = [];
  const walk = (d) => { for (const n of readdirSync(d)) { const p = join(d, n); if (statSync(p).isDirectory()) walk(p); else if (p.endsWith('.js')) files.push(p); } };
  walk(root);
  const used = new Set();
  for (const f of files) {
    const src = readFileSync(f, 'utf8');
    for (const m of src.matchAll(/icon\('([a-z0-9-]+)'/g)) used.add(m[1]);
    for (const m of src.matchAll(/icon: '([a-z0-9-]+)'/g)) used.add(m[1]);
    assert.ok(!/innerHTML|outerHTML|insertAdjacentHTML|eval\(|new Function|style="/.test(src), `unsafe construct in ${f}`);
  }
  for (const name of used) assert.ok(iconNames.includes(name), `missing icon ${name}`);
  assert.ok(used.size > 20);
});
