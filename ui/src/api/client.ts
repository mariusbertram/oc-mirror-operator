import type {
  CatalogPackage,
  ImageFailuresResponse,
  TargetDetail,
  TargetSummary,
} from './types';

// In the dashboard, fetch goes directly to /api (same origin via oauth-proxy).
// In the console plugin, consoleFetch is injected from the SDK and handles auth.
type FetchFn = (url: string, init?: RequestInit) => Promise<Response>;

let _fetch: FetchFn = (url, init) => fetch(url, init);

export function setFetchImpl(fn: FetchFn) {
  _fetch = fn;
}

async function get<T>(path: string): Promise<T> {
  const resp = await _fetch(path);
  if (!resp.ok) {
    throw new Error(`${resp.status} ${resp.statusText}: ${path}`);
  }
  return resp.json() as Promise<T>;
}

async function patch<T>(path: string, body: unknown): Promise<T> {
  const resp = await _fetch(path, {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!resp.ok) {
    const text = await resp.text();
    throw new Error(`${resp.status} ${resp.statusText}: ${text}`);
  }
  return resp.json() as Promise<T>;
}

async function post<T>(path: string, body: unknown): Promise<T> {
  const resp = await _fetch(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!resp.ok) {
    const text = await resp.text();
    throw new Error(`${resp.status} ${resp.statusText}: ${text}`);
  }
  return resp.json() as Promise<T>;
}

async function del(path: string): Promise<void> {
  const resp = await _fetch(path, { method: 'DELETE' });
  if (!resp.ok) {
    const text = await resp.text();
    throw new Error(`${resp.status} ${resp.statusText}: ${text}`);
  }
}

// --- Read API ---

export const listTargets = () => get<TargetSummary[]>('/api/v1/targets');

export const getTarget = (name: string) => get<TargetDetail>(`/api/v1/targets/${name}`);

export const getImageFailures = (targetName: string) =>
  get<ImageFailuresResponse>(`/api/v1/targets/${targetName}/image-failures`);

export const getFilteredPackages = (targetName: string, slug: string) =>
  get<CatalogPackage[]>(`/api/v1/targets/${targetName}/catalogs/${slug}/packages.json`);

export const getUpstreamPackages = (targetName: string, slug: string) =>
  get<CatalogPackage[]>(`/api/v1/targets/${targetName}/catalogs/${slug}/upstream-packages.json`);

// --- Edit API ---

export interface PackagePatchBody {
  include: string[];
  exclude: string[];
}

export const patchCatalogPackages = (
  namespace: string,
  imageSetName: string,
  slug: string,
  body: PackagePatchBody,
) => patch<void>(`/api/v1/imagesets/${namespace}/${imageSetName}/catalogs/${slug}/packages`, body);

export const triggerRecollect = (namespace: string, imageSetName: string) =>
  patch<void>(`/api/v1/imagesets/${namespace}/${imageSetName}/recollect`, {});

export const deleteImageSet = (namespace: string, imageSetName: string) =>
  del(`/api/v1/imagesets/${namespace}/${imageSetName}`);
