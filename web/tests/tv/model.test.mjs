// TV app pure helpers: routing, program labels, guide geometry and keyboard navigation,
// channel stepping and number entry, source selection, lineup item helpers, resume/next.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  TABS, isId, parseRoute, episodeCode, programTitle, programSubtitle, programLabel, programProgress, currentAndNext,
  liveOffset, chooseSource, floorToSlot, guideSlots, guideBox, guideNeighbor, sortChannels, channelStep, pushDigit,
  channelByNumber, ITEM_KINDS, describeItem, itemToInput, moveItem, normalizePicked, pickedTitles, resumeFrom,
  isCompleted, nextEpisode, swipeDirection, clampVolume, guideStart, GUIDE_LEAD_MIN, SLOT_MIN, titleRepeatsShow,
  sizedPoster, stagePoster,
} from '../../assets/js/apps/tv/model.js';
import { iconNames } from '../../assets/js/core/icons.js';

const ID = 'abcdefghijklmnopqrstuvwxyz';
const ID2 = 'zyxwvutsrqponmlkjihgfedcba';

test('routes', () => {
  assert.deepEqual(parseRoute(''), { view: 'live', tab: 'live' });
  assert.deepEqual(parseRoute('guide'), { view: 'guide', tab: 'guide' });
  assert.deepEqual(parseRoute(`watch/${ID}`), { view: 'watch', id: ID, tab: '' });
  assert.deepEqual(parseRoute('watch'), { view: 'watch', id: '', tab: '' });
  assert.deepEqual(parseRoute(`play/${ID}`), { view: 'play', id: ID, tab: '' });
  assert.deepEqual(parseRoute('lineups'), { view: 'lineups', tab: 'lineups' });
  assert.deepEqual(parseRoute(`lineups/${ID}/`), { view: 'lineup', id: ID, tab: 'lineups' });
  assert.deepEqual(parseRoute('channels'), { view: 'channels', tab: 'channels' });
  assert.deepEqual(parseRoute('integrations'), { view: 'integrations', tab: 'integrations' });
  assert.deepEqual(parseRoute('library/show/the-office'), { view: 'library', rest: 'show/the-office', tab: 'library' });
  assert.deepEqual(parseRoute('library'), { view: 'library', rest: '', tab: 'library' });
  for (const bad of ['watch/x', 'play', 'play/../../etc', 'lineups/a/b', 'nope', `guide/${ID}`]) {
    assert.equal(parseRoute(bad).view, 'notfound', bad);
  }
  assert.ok(isId(ID) && !isId('short') && !isId('has space in it') && !isId(null));
  assert.deepEqual(TABS.map((t) => t.id), ['live', 'guide', 'library', 'lineups', 'channels', 'integrations']);
  for (const t of TABS) assert.ok(iconNames.includes(t.icon), t.icon);
  for (const k of Object.values(ITEM_KINDS)) assert.ok(iconNames.includes(k.icon), k.icon);
});

test('program labels and progress', () => {
  const ep = { title: 'Pilot', show_title: 'Show', season: 1, episode: 2, episode_title: 'Pilot', starts_at: '2026-10-26T12:00:00.000Z', ends_at: '2026-10-26T12:30:00.000Z' };
  assert.equal(episodeCode(ep), 'S01E02');
  assert.equal(episodeCode({ episode: 5 }), 'E05');
  assert.equal(episodeCode({ season: 0, episode: 1 }), 'S00E01');
  assert.equal(episodeCode({}), '');
  assert.equal(programTitle(ep), 'Show');
  assert.equal(programSubtitle(ep), 'S01E02 · Pilot');
  assert.equal(programLabel(ep), 'Show · S01E02 · Pilot');
  const movie = { title: 'Alien', show_title: '', season: null, episode: null, episode_title: '' };
  assert.equal(programLabel(movie), 'Alien');
  assert.equal(programSubtitle(movie), '');
  assert.equal(programTitle({}), 'Untitled');
  const start = Date.parse(ep.starts_at);
  assert.equal(programProgress(ep, start), 0);
  assert.equal(programProgress(ep, start + 15 * 60000), 0.5);
  assert.equal(programProgress(ep, start + 3600000), 1);
  assert.equal(programProgress({ starts_at: 'x', ends_at: 'y' }, start), 0);
});

test('current and next from a guide row', () => {
  const at = (h, m) => Date.UTC(2026, 9, 26, h, m);
  const p = (s, e, id) => ({ file_id: id, starts_at: new Date(s).toISOString(), ends_at: new Date(e).toISOString() });
  const row = [p(at(12, 0), at(12, 30), 'a'), p(at(12, 30), at(13, 0), 'b'), p(at(13, 0), at(13, 30), 'c')];
  assert.deepEqual(currentAndNext(row, at(12, 45)), { current: row[1], next: row[2] });
  assert.deepEqual(currentAndNext(row, at(13, 10)), { current: row[2], next: null });
  assert.deepEqual(currentAndNext(row, at(11, 0)), { current: null, next: row[0] });
  assert.deepEqual(currentAndNext([], at(12, 0)), { current: null, next: null });
});

test('live offset and source selection', () => {
  const now = { program: { duration_seconds: 100, stream_url: '/s', transcode_url: '/t', browser_playable: true }, offset_seconds: 10 };
  assert.equal(liveOffset(now, 1000, 6000), 15);
  assert.equal(liveOffset(now, 1000, 1000 + 200000), 99.75);
  assert.equal(liveOffset({ program: null }, 0, 10), 0);
  assert.deepEqual(chooseSource(now.program, 42.37), { url: '/s', kind: 'direct', base: 0, seek: 42.37 });
  assert.deepEqual(chooseSource({ ...now.program, browser_playable: false }, 42.37), { url: '/t?start=42.3', kind: 'transcode', base: 42.3, seek: 0 });
  assert.deepEqual(chooseSource(now.program, 5, { forceTranscode: true }), { url: '/t?start=5', kind: 'transcode', base: 5, seek: 0 });
  // No transcoder: fall back to the direct stream.
  assert.deepEqual(chooseSource({ ...now.program, browser_playable: false, transcode_url: null }, 7), { url: '/s', kind: 'direct', base: 0, seek: 7 });
});

test('guide geometry and slots', () => {
  const t0 = Date.UTC(2026, 9, 26, 12, 0);
  assert.equal(floorToSlot(t0 + 29 * 60000 + 59999), t0);
  assert.equal(floorToSlot(t0 + 30 * 60000), t0 + 30 * 60000);
  assert.deepEqual(guideSlots(t0 + 10 * 60000, t0 + 90 * 60000), [t0, t0 + 1800000, t0 + 3600000]);
  const end = t0 + 3 * 3600000;
  const prog = (s, e) => ({ starts_at: new Date(t0 + s * 60000).toISOString(), ends_at: new Date(t0 + e * 60000).toISOString() });
  assert.deepEqual(guideBox(prog(30, 60), t0, end, 4), { left: 120, width: 120, clippedStart: false, clippedEnd: false });
  assert.deepEqual(guideBox(prog(-20, 10), t0, end, 4), { left: 0, width: 40, clippedStart: true, clippedEnd: false });
  assert.deepEqual(guideBox(prog(170, 200), t0, end, 4), { left: 680, width: 40, clippedStart: false, clippedEnd: true });
  assert.equal(guideBox(prog(-60, -30), t0, end, 4), null);
  assert.equal(guideBox(prog(180, 200), t0, end, 4), null);
});

test('guide keyboard navigation', () => {
  const t0 = Date.UTC(2026, 9, 26, 12, 0);
  const prog = (s, e) => ({ starts_at: new Date(t0 + s * 60000).toISOString(), ends_at: new Date(t0 + e * 60000).toISOString() });
  const rows = [
    [prog(0, 30), prog(30, 60), prog(60, 120)],
    [],
    [prog(0, 45), prog(45, 90)],
  ];
  assert.deepEqual(guideNeighbor(rows, 0, 1, 'right'), { row: 0, index: 2 });
  assert.equal(guideNeighbor(rows, 0, 2, 'right'), null);
  assert.equal(guideNeighbor(rows, 0, 0, 'left'), null);
  // Down skips the empty row and lands on the program airing at the focus time.
  assert.deepEqual(guideNeighbor(rows, 0, 1, 'down', t0 + 50 * 60000), { row: 2, index: 1 });
  assert.deepEqual(guideNeighbor(rows, 2, 0, 'up', t0 + 10 * 60000), { row: 0, index: 0 });
  assert.equal(guideNeighbor(rows, 0, 0, 'up', t0), null);
  assert.deepEqual(guideNeighbor(rows, 0, 2, 'down', t0 + 500 * 60000), { row: 2, index: 1 });
});

test('channel navigation and number entry', () => {
  const chans = [{ id: 'c3', number: 3, name: 'C' }, { id: 'c1', number: 1, name: 'A' }, { id: 'c10', number: 10, name: 'J' }];
  assert.deepEqual(sortChannels(chans).map((c) => c.number), [1, 3, 10]);
  assert.equal(channelStep(chans, 'c1', 1).id, 'c3');
  assert.equal(channelStep(chans, 'c10', 1).id, 'c1');
  assert.equal(channelStep(chans, 'c1', -1).id, 'c10');
  assert.equal(channelStep(chans, 'missing', 1).id, 'c1');
  assert.equal(channelStep([], 'c1', 1), null);
  let buf = '';
  for (const d of ['0', '1', '0', 'x', '5']) buf = pushDigit(buf, d);
  assert.equal(buf, '105');
  assert.equal(pushDigit('1234', '5'), '2345');
  assert.equal(channelByNumber(chans, '10').id, 'c10');
  assert.equal(channelByNumber(chans, 4), null);
});

test('lineup item helpers', () => {
  assert.deepEqual(describeItem({ kind: 'show', show_title: 'Lost' }), { icon: 'tv', title: 'Lost', detail: 'All episodes' });
  assert.deepEqual(describeItem({ kind: 'season', show_title: 'Lost', season: 0 }), { icon: 'layers', title: 'Lost', detail: 'Specials' });
  assert.equal(describeItem({ kind: 'season', show_title: 'Lost', season: 2 }).detail, 'Season 2');
  assert.deepEqual(describeItem({ kind: 'file', file_id: ID }, { [ID]: 'Alien' }), { icon: 'film', title: 'Alien', detail: 'File' });
  assert.equal(describeItem({ kind: 'file', file_id: ID, label: 'Mine' }).title, 'Mine');
  assert.deepEqual(describeItem({ kind: 'folder', path: 'Movies/Classics', recursive: true }), { icon: 'folder', title: 'Classics', detail: '/Movies/Classics · with subfolders' });
  assert.deepEqual(describeItem({ kind: 'folder', path: '', recursive: false }), { icon: 'folder', title: 'Space root', detail: '/' });
  const q = describeItem({ kind: 'query', query: { category: 'movie', genre: 'Horror', year: 1979 } });
  assert.equal(q.title, 'Horror');
  assert.equal(q.detail, 'Query · movie · genre Horror · 1979');
  assert.equal(describeItem({ kind: 'query', query: {} }).title, 'Movies & TV');

  assert.deepEqual(itemToInput({ id: ID, kind: 'show', show_title: 'Lost', order: 'default', label: '', limit: null, extra: 1 }),
    { id: ID, kind: 'show', show_title: 'Lost', order: 'default' });
  assert.deepEqual(moveItem(['a', 'b', 'c', 'd'], 0, 2), ['b', 'c', 'a', 'd']);
  assert.deepEqual(moveItem(['a', 'b', 'c'], 2, 0), ['c', 'a', 'b']);
  assert.deepEqual(moveItem(['a', 'b'], 1, 9), ['a', 'b']);
  assert.deepEqual(moveItem(['a', 'b'], 5, 0), ['a', 'b']);

  const picked = [
    { kind: 'season', show_title: 'Lost', season: 1, id: 'dropme' },
    { id: ID, name: 'Alien.mp4', media: { title: 'Alien' } },
    { show_title: 'Lost', show_key: 'lost', seasons: 6 },
    { show_title: 'Lost', season: 3 },
    { file_id: ID2, title: 'Zulu' },
    { id: ID2, is_dir: true },
    null, 'junk',
  ];
  assert.deepEqual(normalizePicked(picked), [
    { kind: 'season', show_title: 'Lost', season: 1 },
    { kind: 'file', file_id: ID },
    { kind: 'show', show_title: 'Lost' },
    { kind: 'season', show_title: 'Lost', season: 3 },
    { kind: 'file', file_id: ID2 },
  ]);
  assert.deepEqual(pickedTitles(picked), { [ID]: 'Alien', [ID2]: 'Zulu' });
});

test('resume and next episode', () => {
  assert.equal(resumeFrom({ media: { position_seconds: 5, duration_seconds: 100 } }), 0);
  assert.equal(resumeFrom({ media: { position_seconds: 50, duration_seconds: 100 } }), 50);
  assert.equal(resumeFrom({ media: { position_seconds: 97, duration_seconds: 100 } }), 0);
  assert.equal(resumeFrom({ media: { position_seconds: 500, duration_seconds: null } }), 500);
  assert.ok(isCompleted(96, 100) && !isCompleted(50, 100) && !isCompleted(5, 0));
  const ep = (id, s, e, show = 'Lost') => ({ id, name: id, media: { show_title: show, season: s, episode: e } });
  const list = [ep('e3', 1, 3), ep('e1', 1, 1), ep('s2', 2, 1), ep('e2', 1, 2), ep('x', 1, 4, 'Other')];
  assert.equal(nextEpisode(list, ep('e1', 1, 1)).id, 'e2');
  assert.equal(nextEpisode(list, ep('e3', 1, 3)).id, 's2');
  assert.equal(nextEpisode(list, ep('s2', 2, 1)), null);
  assert.equal(nextEpisode(list, { id: 'm', media: { show_title: '' } }), null);
});

test('gestures and volume', () => {
  assert.equal(swipeDirection(5, -80), 'up');
  assert.equal(swipeDirection(-3, 90), 'down');
  assert.equal(swipeDirection(-100, 10), 'left');
  assert.equal(swipeDirection(10, 10), null);
  assert.equal(swipeDirection(60, 55), null);
  assert.equal(clampVolume(1.5), 1);
  assert.equal(clampVolume(-1), 0);
  assert.equal(clampVolume(0.333), 0.33);
  assert.equal(clampVolume('x'), 1);
});

test('guide window keeps "now" away from its left edge', () => {
  // Regression: at 12:30:10 the window started at 12:30, cutting the NOW marker in half.
  const t0 = Date.UTC(2026, 9, 26, 12, 0);
  for (const minute of [0, 0.2, 14.9, 15, 29.9, 30, 30.2, 44, 59.9]) {
    const now = t0 + minute * 60000;
    const start = guideStart(now);
    assert.equal(start % (SLOT_MIN * 60000), 0, 'starts on a slot boundary');
    const lead = (now - start) / 60000;
    assert.ok(lead >= GUIDE_LEAD_MIN && lead < GUIDE_LEAD_MIN + SLOT_MIN, `${minute}: lead ${lead}`);
  }
  assert.equal(guideStart(t0 + 30 * 60000 + 10000), t0, '12:30:10 → 12:00');
  assert.equal(guideStart(t0 + 50 * 60000), t0 + 30 * 60000, '12:50 → 12:30');
});

test('episode titles that only repeat the show are not shown twice', () => {
  // Regression: "Garden Hour · S01E02 · Garden Hour S01E02" for files without an episode title.
  const ep = (title, extra = {}) => ({ title, show_title: 'Garden Hour', season: 1, episode: 2, episode_title: '', ...extra });
  assert.equal(programLabel(ep('Garden Hour S01E02')), 'Garden Hour · S01E02');
  assert.equal(programLabel(ep('Garden.Hour.S01E02')), 'Garden Hour · S01E02');
  assert.equal(programLabel(ep('garden hour - 1x02')), 'Garden Hour · S01E02');
  assert.equal(programLabel(ep('Garden Hour')), 'Garden Hour · S01E02');
  assert.equal(programLabel(ep('Garden Hour E02', { season: null })), 'Garden Hour · E02');
  // Real titles stay, including ones that start with the show name.
  assert.equal(programLabel(ep('Roses')), 'Garden Hour · S01E02 · Roses');
  assert.equal(programLabel(ep('Garden Hour Live')), 'Garden Hour · S01E02 · Garden Hour Live');
  assert.equal(programLabel(ep('Garden Hour S01E03')), 'Garden Hour · S01E02 · Garden Hour S01E03');
  assert.equal(programLabel(ep('x', { episode_title: 'The Signal' })), 'Garden Hour · S01E02 · The Signal');
  assert.equal(titleRepeatsShow('Show S1E2', 'Show', 1, 2), true);
  assert.equal(titleRepeatsShow('Show S1E2', 'Show', null, null), false);
});

test('player stage poster', () => {
  assert.equal(sizedPoster(null), null);
  assert.equal(sizedPoster('/p/poster'), '/p/poster?size=lg');
  assert.equal(sizedPoster('/p/poster?v=abc'), '/p/poster?v=abc&size=lg');
  assert.equal(sizedPoster('/p/poster?size=md'), '/p/poster?size=lg');
  assert.equal(sizedPoster('/p/poster?size=md&access_token=x', 'sm'), '/p/poster?size=sm&access_token=x');
  const ch = { logo_url: '/api/v1/tv/channels/c1/logo?v=1' };
  assert.equal(stagePoster({ poster_url: '/api/v1/tv/channels/c1/programs/f1/poster?size=md' }, ch), '/api/v1/tv/channels/c1/programs/f1/poster?size=lg');
  // Audio programs without artwork fall back to the channel logo instead of a black stage.
  assert.equal(stagePoster({ poster_url: null }, ch), ch.logo_url);
  assert.equal(stagePoster({ poster_url: null }, { logo_url: null }), null);
  assert.equal(stagePoster(null, null), null);
});
