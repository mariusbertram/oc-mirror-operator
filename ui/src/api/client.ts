import type {
  CatalogPackagesResponse,
  ImageFailuresResponse,
  OcpChannelEntry,
  PackageConstraint,
  ReleaseSpec,
  TargetDetail,
  TargetSummary,
} from './types';

// In the dashboard, fetch goes directly to /api (same origin via oauth-proxy).
// In the console plugin, consoleFetch is injected from the SDK and handles auth.
type FetchFn = (url: string, init?: RequestInit) => Promise<Response>;

let _fetch: FetchFn = (url, init) => fetch(url, init);
let _baseUrl = '';

export function setFetchImpl(fn: FetchFn) {
  _fetch = fn;
}

// setApiBaseUrl sets a prefix prepended to every API path.
// Plugin pages set this to the ConsolePlugin proxy path so that requests
// reach the backend via the OpenShift Console proxy mechanism.
export function setApiBaseUrl(baseUrl: string) {
  _baseUrl = baseUrl;
}

export function getApiUrl(path: string): string {
  return _baseUrl + path;
}

async function get<T>(path: string): Promise<T> {
  console.log('API GET:', _baseUrl + path);
  const resp = await _fetch(_baseUrl + path);
  if (!resp.ok) {
    console.error('API GET failed:', _baseUrl + path, resp.status, resp.statusText);
    throw new Error(`${resp.status} ${resp.statusText}: ${path}`);
  }
  const text = await resp.text();
  if (!text) return {} as T;
  return JSON.parse(text) as T;
}

async function patch<T>(path: string, body: unknown): Promise<T> {
  console.log('API PATCH:', _baseUrl + path);
  const resp = await _fetch(_baseUrl + path, {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!resp.ok) {
    const text = await resp.text();
    console.error('API PATCH failed:', _baseUrl + path, resp.status, resp.statusText, text);
    throw new Error(`${resp.status} ${resp.statusText}: ${text}`);
  }
  const text = await resp.text();
  if (!text) return {} as T;
  return JSON.parse(text) as T;
}

async function post<T>(path: string, body: unknown): Promise<T> {
  console.log('API POST:', _baseUrl + path);
  const resp = await _fetch(_baseUrl + path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!resp.ok) {
    const text = await resp.text();
    console.error('API POST failed:', _baseUrl + path, resp.status, resp.statusText, text);
    throw new Error(`${resp.status} ${resp.statusText}: ${text}`);
  }
  const text = await resp.text();
  if (!text) return {} as T;
  return JSON.parse(text) as T;
}

async function del(path: string): Promise<void> {
  console.log('API DELETE:', _baseUrl + path);
  const resp = await _fetch(_baseUrl + path, { method: 'DELETE' });
  if (!resp.ok) {
    const text = await resp.text();
    console.error('API DELETE failed:', _baseUrl + path, resp.status, resp.statusText, text);
    throw new Error(`${resp.status} ${resp.statusText}: ${text}`);
  }
  // Consume the body even for DELETE to avoid potential fetch issues
  await resp.text();
}

// --- Read API ---

export const listTargets = () => get<TargetSummary[]>('/api/v1/targets');

function normalizeTargetDetail(t: TargetDetail): TargetDetail {
  return {
    ...t,
    conditions: t.conditions ?? [],
    imageSets: (t.imageSets ?? []).map((is) => ({
      ...is,
      resources: is.resources ?? [],
    })),
    resources: t.resources ?? [],
    catalogs: t.catalogs ?? [],
  };
}

export const getTarget = (name: string) =>
  get<TargetDetail>(`/api/v1/targets/${name}`).then(normalizeTargetDetail);

export const getImageFailures = (targetName: string) =>
  get<ImageFailuresResponse>(`/api/v1/targets/${targetName}/image-failures`);

export const getFilteredPackages = (targetName: string, slug: string) =>
  get<CatalogPackagesResponse>(`/api/v1/targets/${targetName}/catalogs/${slug}/packages.json`);

export const getUpstreamPackages = (targetName: string, slug: string) =>
  get<CatalogPackagesResponse>(`/api/v1/targets/${targetName}/catalogs/${slug}/upstream-packages.json`);

export const getPackageConstraints = (namespace: string, imageSetName: string, slug: string) =>
  get<PackageConstraint[]>(`/api/v1/imagesets/${namespace}/${imageSetName}/catalogs/${slug}/packages`);

// --- Edit API ---

export interface PackagePatchBody {
  packages: PackageConstraint[];
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

export async function fetchRawText(path: string): Promise<string> {
  const resp = await _fetch(_baseUrl + path);
  if (!resp.ok) throw new Error(`${resp.status} ${resp.statusText}`);
  return resp.text();
}

export const getReleases = (namespace: string, imageSetName: string) =>
  get<ReleaseSpec>(`/api/v1/imagesets/${namespace}/${imageSetName}/releases`);

export const patchReleases = (namespace: string, imageSetName: string, body: ReleaseSpec) =>
  patch<void>(`/api/v1/imagesets/${namespace}/${imageSetName}/releases`, body);

/** Fetch the list of available OCP/OKD release channels.
 *  The backend queries openshift/cincinnati-graph-data (cached 1h),
 *  falls back to the oc-mirror-ocp-versions ConfigMap, then hardcoded defaults. */
export const getOcpChannels = () => get<OcpChannelEntry[]>('/api/v1/releases/channels');
