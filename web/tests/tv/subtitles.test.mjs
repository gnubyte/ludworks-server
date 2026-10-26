// Program subtitle tracks in the live player (Program.subtitles → <track> descriptors, the
// remembered caption choice, cue alignment for transcoded sources).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { programTracks, pickCaption, captionKey, shiftCue } from '../../assets/js/apps/tv/model.js';

const base = '/api/v1/tv/channels/ch1234567890/programs/file12345678/subtitles/';

test('programTracks maps Program.subtitles to <track> descriptors', () => {
  const p = {
    subtitles: [
      { index: 0, language: 'en', label: 'English', url: `${base}0.vtt` },
      { index: 2, language: 'pt-BR', label: '', url: `${base}2.vtt` },
      { index: 3, language: 'Klingon!', label: 'Klingon', url: `${base}3.vtt` },
      { index: 4, language: '', label: '', url: `${base}4.vtt` },
      { index: 5, language: 'fr', label: 'Evil', url: 'https://evil.example/x.vtt' },
      { index: 6, language: 'de', label: 'Other path', url: '/api/v1/files/abc/content' },
      null,
    ],
  };
  assert.deepEqual(programTracks(p), [
    { src: `${base}0.vtt`, label: 'English', srclang: 'en', key: 'lang:en' },
    { src: `${base}2.vtt`, label: 'pt-BR', srclang: 'pt-BR', key: 'lang:pt-br' },
    { src: `${base}3.vtt`, label: 'Klingon', srclang: undefined, key: 'label:klingon' },
    { src: `${base}4.vtt`, label: 'Track 4', srclang: undefined, key: 'label:track 4' },
  ]);
  assert.deepEqual(programTracks({}), []);
  assert.deepEqual(programTracks(null), []);
  assert.deepEqual(programTracks({ subtitles: 'nope' }), []);
});

test('pickCaption carries the choice over by language, then label', () => {
  const tracks = programTracks({
    subtitles: [
      { index: 0, language: 'de', label: 'Deutsch', url: `${base}0.vtt` },
      { index: 1, language: 'en-GB', label: 'English (UK)', url: `${base}1.vtt` },
      { index: 2, language: '', label: 'Commentary', url: `${base}2.vtt` },
    ],
  });
  assert.equal(pickCaption(tracks, ''), -1);
  assert.equal(pickCaption(tracks, null), -1);
  assert.equal(pickCaption(tracks, 'lang:de'), 0);
  assert.equal(pickCaption(tracks, 'lang:en-gb'), 1);
  assert.equal(pickCaption(tracks, 'lang:en'), 1, 'en matches en-GB');
  assert.equal(pickCaption(tracks, 'lang:fr'), -1);
  assert.equal(pickCaption(tracks, 'label:commentary'), 2);
  assert.equal(pickCaption(tracks, 'label:nope'), -1);
  assert.equal(pickCaption([], 'lang:de'), -1);
  assert.equal(captionKey({ srclang: 'EN', label: 'x' }), 'lang:en');
  assert.equal(captionKey({ label: 'Director' }), 'label:director');
  assert.equal(captionKey(null), '');
});

test('shiftCue moves file-relative cue times to a transcode starting at base', () => {
  assert.deepEqual(shiftCue(10, 12, 0), [10, 12]);
  assert.deepEqual(shiftCue(130, 132.5, 120.5), [9.5, 12]);
  assert.deepEqual(shiftCue(5, 8, 100), [0, 0]);
  assert.deepEqual(shiftCue(99, 101, 100), [0, 1]);
  assert.deepEqual(shiftCue(3, 4, undefined), [3, 4]);
});
