import { describe, expect, it } from 'vitest';
import {
  formatAgo,
  formatLimit,
  formatMS,
  formatPercent,
  formatTime,
  formatUSD,
  percentOf,
  shortId,
  traceHref,
} from './format';

describe('formatUSD', () => {
  it.each([
    [0, '$0.00'],
    [0.000123, '$0.000123'],
    [0.0421, '$0.0421'],
    [1.2345, '$1.23'],
    [250, '$250.00'],
    [Number.NaN, '–'],
  ])('%s -> %s', (n, want) => {
    expect(formatUSD(n)).toBe(want);
  });
});

describe('formatMS', () => {
  it.each([
    [0, '–'],
    [undefined, '–'],
    [4.24, '4.2 ms'],
    [812.4, '812 ms'],
    [1240, '1.24 s'],
  ])('%s -> %s', (n, want) => {
    expect(formatMS(n)).toBe(want);
  });
});

describe('formatPercent', () => {
  it('formats a fraction', () => {
    expect(formatPercent(0.0238)).toBe('2.4%');
    expect(formatPercent(1)).toBe('100.0%');
  });
});

describe('formatAgo', () => {
  it.each([
    [0.2, 'just now'],
    [3, '3 s ago'],
    [95, '1 m ago'],
    [7300, '2 h ago'],
    [200000, '2 d ago'],
  ])('%s -> %s', (s, want) => {
    expect(formatAgo(s)).toBe(want);
  });
});

describe('formatTime', () => {
  it('prints local HH:MM:SS.mmm', () => {
    const iso = '2026-11-10T12:34:56.078Z';
    const d = new Date(iso);
    const p = (n: number, w = 2) => String(n).padStart(w, '0');
    expect(formatTime(iso)).toBe(
      `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}.${p(d.getMilliseconds(), 3)}`,
    );
    expect(formatTime(iso, false)).toMatch(/^\d\d:\d\d:\d\d$/);
  });
  it('returns the input when it is not a date', () => {
    expect(formatTime('nope')).toBe('nope');
  });
});

describe('formatLimit', () => {
  it('formats rate, period and burst', () => {
    expect(formatLimit({ rate: 100, period_seconds: 60, burst: 20 })).toBe('100 / 60 s, burst 20');
    expect(formatLimit({ rate: 50000, period_seconds: 60, burst: 10000 }, 'tokens')).toBe(
      '50,000 tokens / 60 s, burst 10,000',
    );
  });
});

describe('percentOf', () => {
  it('rounds and tolerates a zero limit', () => {
    expect(percentOf(1.25, 5)).toBe(25);
    expect(percentOf(1, 0)).toBe(0);
    expect(percentOf(7, 5)).toBe(140);
  });
});

describe('traceHref', () => {
  it('substitutes the id', () => {
    expect(traceHref('http://j/trace/{trace_id}', 'abc')).toBe('http://j/trace/abc');
    expect(traceHref('/t?id={trace_id}&x={trace_id}', 'a b')).toBe('/t?id=a%20b&x=a%20b');
  });
  it('is undefined without template, id, or a safe scheme', () => {
    expect(traceHref(undefined, 'abc')).toBeUndefined();
    expect(traceHref('http://j/{trace_id}', undefined)).toBeUndefined();
    expect(traceHref('javascript:alert({trace_id})', 'abc')).toBeUndefined();
  });
});

describe('shortId', () => {
  it('truncates long ids only', () => {
    expect(shortId('4bf92f3577b34da6a3ce929d0e0e4736')).toBe('4bf92f35');
    expect(shortId('abc')).toBe('abc');
  });
});
