// /tv/integrations — feed URLs for external players and media servers (M3U, XMLTV,
// HDHomeRun) with copy buttons, token rotation, setup guides and ffmpeg notes.

import { h, uid } from '../../core/dom.js';
import { tvIcon } from './icons.js';

const GUIDES = [
  {
    id: 'vlc', title: 'VLC', icon: 'play',
    steps: [
      'Open VLC and choose Media → Open Network Stream (Ctrl+N).',
      'Paste the M3U playlist URL and press Play.',
      'Open the playlist (Ctrl+L) to switch channels. VLC does not read XMLTV, so there is no guide.',
    ],
  },
  {
    id: 'plex', title: 'Plex', icon: 'tv',
    steps: [
      'Ask an administrator to enable HDHomeRun emulation (Admin → Media & TV).',
      'In Plex: Settings → Live TV & DVR → Set up Plex DVR. If the tuner is not found, enter the HDHomeRun URL manually.',
      'When asked for a guide, choose “Have an XMLTV guide on your server?” and paste the XMLTV URL.',
      'Live streams need ffmpeg on the Ludworks server (see below).',
    ],
  },
  {
    id: 'jellyfin', title: 'Jellyfin / Emby', icon: 'server',
    steps: [
      'Dashboard → Live TV → Tuner devices → Add: choose “M3U Tuner” and paste the M3U URL (or choose “HDHomeRun” and paste the HDHomeRun URL).',
      'TV guide data providers → Add → XMLTV: paste the XMLTV URL.',
      'Refresh the guide data. Channel numbers and logos come from the playlist.',
    ],
  },
  {
    id: 'tivimate', title: 'TiviMate / IPTV apps', icon: 'smartphone',
    steps: [
      'Add playlist → Enter URL: paste the M3U URL. The EPG URL is picked up automatically (url-tvg); otherwise add the XMLTV URL as EPG source.',
      'Pick a refresh interval of a day or so — schedules are computed by Ludworks, nothing needs re-downloading often.',
    ],
  },
];

export async function render(host, { ctx, api, ui, signal }) {
  const body = h('div', { class: 'stack-lg' });
  const page = h('div', { class: 'page tv-integrations' },
    ui.pageHeader({
      title: 'Integrations',
      subtitle: 'Watch your channels in VLC, Plex, Jellyfin, Kodi or any IPTV app.',
    }),
    body);
  host.append(page);
  body.append(ui.skeletonCards(2, '120px'));

  let feeds;
  try {
    feeds = await api.feeds(signal);
  } catch (err) {
    if (signal.aborted) return null;
    body.replaceChildren(err?.status === 403
      ? ui.alert({ variant: 'warning', title: 'Not available with this sign-in', message: ui.errorMessage(err) })
      : ui.errorState(err, () => location.reload()));
    return null;
  }

  // ffmpeg availability: a program with a transcode URL means the server has ffmpeg.
  let ffmpeg = null;
  try {
    const chans = await api.channels(signal);
    for (const c of chans.slice(0, 5)) {
      const n = await api.now(c.id, signal);
      if (n.program) {
        ffmpeg = !!n.program.transcode_url;
        break;
      }
    }
  } catch { /* unknown */ }

  function urlRow(label, url, hint) {
    const id = uid('feed');
    const input = h('input', { class: 'input mono tv-feed-input', id, readonly: true, value: url, spellcheck: 'false', autocomplete: 'off' });
    input.addEventListener('focus', () => input.select());
    return h('div', { class: 'field tv-feed-row' },
      h('label', { class: 'field-label', for: id }, label),
      h('div', { class: 'input-group' }, input, ui.copyButton(() => input.value, { title: `Copy ${label}`, variant: 'secondary', size: 'md', message: `${label} copied` })),
      hint ? h('div', { class: 'field-hint' }, hint) : null);
  }

  const cardBody = h('div', { class: 'stack-md' });
  const card = ui.card({
    title: 'Your feed URLs', icon: 'link',
    description: 'These links contain a private token that can watch your TV channels (and the shared ones) and read their guide. Anyone with a link can do the same, so treat them like a password and rotate the token if one leaks.',
    actions: ui.button({
      label: 'Rotate token', icon: 'refresh', size: 'sm',
      onClick: async () => {
        const ok = await ui.confirm({ title: 'Rotate the feed token?', message: 'Every app using the current URLs stops working until you paste the new ones.', confirmLabel: 'Rotate', danger: true });
        if (!ok) return;
        try {
          feeds = await api.rotateFeeds();
          draw();
          ui.toast.success('New feed URLs created');
        } catch (err) {
          ui.toast.error(err);
        }
      },
    }),
    body: cardBody,
  });

  function draw() {
    cardBody.replaceChildren(
      urlRow('M3U playlist', feeds.m3u_url, 'Channel list with numbers, logos and groups.'),
      urlRow('XMLTV guide', feeds.xmltv_url, 'Program guide for the next days.'),
      feeds.hdhr_url
        ? urlRow('HDHomeRun tuner', feeds.hdhr_url, 'For Plex, Jellyfin and Emby “HDHomeRun” tuners.')
        : h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'HDHomeRun tuner'),
          ui.alert({ variant: 'info', message: ctx.isAdmin
            ? h('span', {}, 'HDHomeRun emulation is off. ', h('a', { href: '/admin/media' }, 'Enable it in Admin → Media & TV'), '.')
            : 'HDHomeRun emulation is off. Ask an administrator to enable it.' })));
  }
  draw();

  const ffNote = ui.alert({
    variant: ffmpeg === false ? 'warning' : 'info',
    title: ffmpeg === true ? 'ffmpeg found — live streams are available' : ffmpeg === false ? 'ffmpeg not found on the server' : 'About ffmpeg',
    message: ffmpeg === false
      ? 'Without ffmpeg the playlist points each channel at the current program (a redirect). VLC and most IPTV apps handle that, but Plex/Jellyfin tuners and continuous streams (live.ts) need ffmpeg. An administrator can install it or set its path in Admin → Media & TV.'
      : 'Continuous streams (live.ts) and HDHomeRun tuners transcode with ffmpeg on the server: 720p H.264/AAC, one ffmpeg process per viewer. Each user can watch as many streams at once as the tuner count allows.',
  });

  const guides = h('div', { class: 'tv-guides' }, GUIDES.map((g, i) => h('details', { class: 'tv-guide-item card', open: i === 0 ? true : undefined },
    h('summary', {}, h('span', { class: 'tv-guide-item-icon' }, tvIcon(g.icon, 16)), h('span', { class: 'bold' }, g.title), tvIcon('chevron-down', { size: 14, className: 'tv-guide-chevron' })),
    h('ol', { class: 'tv-steps' }, g.steps.map((s) => h('li', {}, s))))));

  body.replaceChildren(card, ffNote, h('section', { class: 'stack-sm' }, h('h2', { class: 'section-title' }, 'Setup guides'), guides),
    h('p', { class: 'small faint' }, 'The feed token is listed among your API tokens (Settings → API tokens) as “TV feeds”. Revoking it there disables every feed URL.'));
  return null;
}
