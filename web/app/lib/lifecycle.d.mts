export type LifecycleSandbox = {
  autoStop?: boolean;
  expiresAt?: string;
  ttlRemainingSeconds?: number;
  ttlSeconds?: number | null;
};

export function autoStopLabel(sandbox: LifecycleSandbox, now?: number): string;
export function extendTTLSeconds(sandbox: LifecycleSandbox, now?: number): number;
export function timerTTLSeconds(selection: string, customSeconds?: number): number | null;
