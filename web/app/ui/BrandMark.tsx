import { CSSProperties } from 'react';

export function BrandMark({ size = 20, animated = false, className = '' }: { size?: number; animated?: boolean; className?: string }) {
  return <svg className={`brand-mark-svg ${animated ? 'brand-mark-loading' : ''} ${className}`} width={size} height={size} viewBox="0 0 32 32" aria-hidden="true" focusable="false">
    <path fill="currentColor" fillRule="evenodd" d="M5 3h4v8h18v18H5V3Zm4 8v14h14V11H9Z" />
    <path className="brand-mark-lid" fill="currentColor" d="M9 11h18v4H9z" />
  </svg>;
}
