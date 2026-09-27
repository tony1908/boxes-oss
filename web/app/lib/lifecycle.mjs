const HOUR = 60 * 60;

function remainingSeconds(sandbox, now) {
  if (sandbox.expiresAt) {
    const expiresAt = Date.parse(sandbox.expiresAt);
    if (Number.isFinite(expiresAt)) return Math.max(0, Math.ceil((expiresAt - now) / 1000));
  }
  if (typeof sandbox.ttlRemainingSeconds === 'number') return Math.max(0, Math.ceil(sandbox.ttlRemainingSeconds));
  if (typeof sandbox.ttlSeconds === 'number') return Math.max(0, Math.ceil(sandbox.ttlSeconds));
  return 0;
}

export function autoStopLabel(sandbox, now = Date.now()) {
  const isAutoStop = sandbox.autoStop ?? Boolean(sandbox.expiresAt || sandbox.ttlRemainingSeconds || sandbox.ttlSeconds);
  if (!isAutoStop) return 'Always on';

  const seconds = remainingSeconds(sandbox, now);
  if (seconds < 60) return 'Auto-stops in less than a minute';
  const minutes = Math.ceil(seconds / 60);
  if (minutes < 60) return `Auto-stops in ${minutes}m`;
  const hours = Math.floor(minutes / 60);
  const remainingMinutes = minutes % 60;
  if (hours < 24) return `Auto-stops in ${hours}h${remainingMinutes ? ` ${remainingMinutes}m` : ''}`;
  const days = Math.floor(hours / 24);
  const remainingHours = hours % 24;
  return `Auto-stops in ${days}d${remainingHours ? ` ${remainingHours}h` : ''}`;
}

export function extendTTLSeconds(sandbox, now = Date.now()) {
  return remainingSeconds(sandbox, now) + HOUR;
}

export function timerTTLSeconds(selection, customSeconds) {
  if (selection === 'always') return null;
  if (selection === 'custom') return Number(customSeconds);
  return Number(selection);
}
