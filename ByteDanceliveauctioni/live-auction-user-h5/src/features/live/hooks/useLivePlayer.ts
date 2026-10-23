const LOCAL_LIVE_DEMO_PLAYLIST = ['/demo-live.mp4'];

function configuredLiveSources() {
  const configuredList = import.meta.env.VITE_DEMO_LIVE_URLS as string | undefined;
  const configuredSingle = import.meta.env.VITE_DEMO_LIVE_URL as string | undefined;
  const list = configuredList
    ?.split(',')
    .map((url) => url.trim())
    .filter(Boolean);

  if (list?.length) return list;
  if (configuredSingle?.trim()) return [configuredSingle.trim()];
  return LOCAL_LIVE_DEMO_PLAYLIST;
}

export function resolveLivePlaylist() {
  const playlist = configuredLiveSources();
  return playlist.length ? playlist : ['/demo-live.mp4'];
}

export function resolveLiveSource() {
  const playlist = resolveLivePlaylist();
  return playlist[0] || '/demo-live.mp4';
}

export function resolveInitialLiveSource() {
  const playlist = resolveLivePlaylist();
  return playlist[Math.floor(Math.random() * playlist.length)] || resolveLiveSource();
}

export function resolveNextLiveSource(currentSource: string) {
  const playlist = resolveLivePlaylist();
  if (playlist.length <= 1) return playlist[0] || '/demo-live.mp4';
  const index = playlist.indexOf(currentSource);
  return playlist[(index + 1 + playlist.length) % playlist.length] || playlist[0];
}

export function isHls(url: string) { return /\.m3u8($|\?)/i.test(url); }
