function formatBytes(value) {
  if (!value) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  const step = Math.min(Math.floor(Math.log(value) / Math.log(1024)), units.length - 1);
  return `${(value / 1024 ** step).toFixed(step > 1 ? 1 : 0).replace(/\.0$/, '')} ${units[step]}`;
}

function sentenceCase(value) {
  const sentence = String(value ?? '').replace(/[._-]+/g, ' ').trim();
  return sentence ? sentence[0].toUpperCase() + sentence.slice(1) : 'Activity recorded';
}

function unwrapCommand(value) {
  let command = String(value ?? '').trim();
  const wrapper = /^(?:bash\s+-lc|sh\s+-c)\s+(?:"([\s\S]*)"|'([\s\S]*)'|([\s\S]*))$/;
  while (wrapper.test(command)) command = command.replace(wrapper, (_match, doubleQuoted, singleQuoted, unquoted) => doubleQuoted ?? singleQuoted ?? unquoted).trim();
  return command;
}

export function activityPresentation(item) {
  const detail = item.detail ?? {};
  const action = String(item.action ?? '').toLowerCase();
  const category = String(item.category ?? '').toLowerCase();
  const number = (key) => Number(detail[key] ?? 0);
  if (action === 'error') {
    const reason = String(detail.reason ?? '').trim();
    return { text: reason ? reason.slice(0, 140) : 'Error', result: 'Failed' };
  }
  if (category === 'command' || action === 'exec') return { text: 'Ran ', code: unwrapCommand(detail.command ?? detail.cmd ?? 'command'), suffix: '' };
  if (action === 'file write' || action === 'file written' || action === 'file read') {
    return { text: action === 'file read' ? 'Read ' : 'Wrote ', code: String(detail.path ?? ''), suffix: ` (${formatBytes(number('sizeBytes'))})` };
  }
  if (action === 'files pushed') return { text: `Pushed files to ${detail.path ?? ''} (${formatBytes(number('sizeBytes'))})` };
  if (action === 'downloaded') return { text: `Downloaded ${detail.path ?? ''}` };
  if (action.startsWith('desktop session ')) return { text: 'Opened desktop' };
  if (action === 'periodic snapshots') return { text: `Took ${number('count')} periodic snapshots` };
  if (action === 'host exposed') return { text: `Exposed port ${number('port')} (${detail.public ? 'public' : 'private'})` };
  if (action === 'ssh key added') return { text: 'Added an SSH key' };
  if (action === 'ssh proxy session opened') return { text: 'Opened SSH session' };
  if (action === 'ssh proxy session closed') return { text: 'Closed SSH session' };
  if (action === 'auto-stopped by timer') return { text: 'Stopped by auto-stop timer' };
  if (/^stopped after \d+ min idle$/.test(action)) return { text: `Stopped after ${action.match(/\d+/)?.[0]} min idle` };
  if (action === 'stopped' || action === 'stop requested') return { text: 'Stopped' };
  if (action === 'resumed') return { text: 'Resumed' };
  if (action === 'forked') return { text: `Forked to ${detail.newBoxId ?? 'sandbox'}` };
  if (action === 'ttl changed') {
    const label = (value) => value == null ? 'Always on' : value < 3600 ? `${value} sec` : `${value / 3600} h`;
    return { text: `Changed auto-stop from ${label(detail.old)} to ${label(detail.new)}` };
  }
  if (action === 'idle stop changed') return { text: detail.new ? `Changed idle stop to ${Math.round(Number(detail.new) / 60)} min` : 'Turned off idle stop' };
  if (category === 'agent' || action.includes('prompt run')) {
    const provider = detail.provider ?? detail.agent ?? '';
    const prompt = String(detail.promptPreview ?? detail.prompt ?? '');
    const preview = prompt ? `: '${prompt.slice(0, 80)}${prompt.length > 80 ? '…' : ''}'` : '';
    return { text: `Agent run${provider ? ` with ${provider}` : ''}${preview}` };
  }
  return { text: sentenceCase(item.action) };
}

export function usageCountsLabel(usage) {
  const count = (value, singular, plural = `${singular}s`) => `${value} ${value === 1 ? singular : plural}`;
  return `${count(usage.commandsCount, 'command')}, ${count(usage.agentRunsCount, 'agent run')}, ${count(usage.desktopSessionsCount, 'desktop session')}, ${count(usage.snapshotsCount, 'snapshot')} kept (latest ${formatBytes(usage.snapshotsBytes)})`;
}
