export interface TargetSummary {
  namespace: string;
  name: string;
  registry: string;
  totalImages: number;
  mirroredImages: number;
  pendingImages: number;
  failedImages: number;
}

export interface ConditionSummary {
  type: string;
  status: string;
  reason: string;
  message: string;
}

export interface ResourceLink {
  name: string;
  url: string;
  type: string;
}

export interface ImageSetSummary {
  name: string;
  found: boolean;
  total: number;
  mirrored: number;
  pending: number;
  failed: number;
  resources: ResourceLink[];
  catalogs: string[];
  hasPlatform?: boolean;
}

export interface CatalogSummary {
  slug: string;
  source: string;
  targetImage: string;
  filteredPackagesUrl: string;
  upstreamPackagesUrl: string;
  imageSets: string[];
}

export interface TargetDetail extends TargetSummary {
  conditions: ConditionSummary[];
  imageSets: ImageSetSummary[];
  resources: ResourceLink[];
  catalogs: CatalogSummary[];
}

export interface FailedImageDetail {
  destination: string;
  source: string;
  state: string;
  lastError?: string;
  retryCount?: number;
  permanentlyFailed?: boolean;
  imageSet: string;
}

export interface ImageFailuresResponse {
  failed: FailedImageDetail[];
  pending: FailedImageDetail[];
}

export interface ChannelConstraint {
  name: string;
  minVersion?: string;
  maxVersion?: string;
}

export interface PackageConstraint {
  name: string;
  minVersion?: string;
  maxVersion?: string;
  channels?: ChannelConstraint[];
}

export interface CatalogChannel {
  name: string;
  entries: {
    name: string;
    version: string;
  }[];
  /** All available version strings in this channel, sorted ascending. */
  versions?: string[];
}

export interface CatalogPackage {
  name: string;
  defaultChannel: string;
  channels: CatalogChannel[];
  description?: string;
}

export interface CatalogPackagesResponse {
  catalog: string;
  targetImage: string;
  packages: CatalogPackage[];
}

export interface ReleaseChannel {
  name: string;
  type?: string; // 'ocp' | 'okd'
  minVersion?: string;
  maxVersion?: string;
  shortestPath?: boolean;
  full?: boolean;
  skipSignatureVerification?: boolean;
}

export interface ReleaseSpec {
  graph?: boolean;
  architectures?: string[];
  channels?: ReleaseChannel[];
}

export interface HelmChart {
  name: string;
  version?: string;
  path?: string;
  imagePaths?: string[];
}

export interface HelmRepository {
  name: string;
  url: string;
  charts: HelmChart[];
}

export interface HelmSpec {
  repositories: HelmRepository[];
}

export interface BlockedImagesSpec {
  blockedImages: string[];
}

export interface AdditionalImageEntry {
  name: string;
  targetRepo?: string;
  targetTag?: string;
}

export interface AdditionalImagesSpec {
  additionalImages: AdditionalImageEntry[];
}

export interface OperatorEntry {
  catalog: string;
  targetCatalog?: string;
  targetTag?: string;
  full?: boolean;
  skipDependencies?: boolean;
  /** Read-only hint: this catalog has package filters configured via the Catalogs tab. */
  packagesConfigured?: boolean;
  /** Read-only hint: this catalog has cosign signature verification configured (kubectl-only). */
  signatureVerificationConfigured?: boolean;
}

export interface OperatorsSpec {
  operators: OperatorEntry[];
}

export interface ImageSetSettings {
  requireSignedImages: boolean;
}

export interface MirrorTargetSpecWire {
  registry: string;
  insecure: boolean;
  authSecret?: string;
  concurrency?: number;
  batchSize?: number;
  /** Go duration string, e.g. "24h0m0s". Empty means "use the controller default". */
  pollInterval?: string;
  /** Go duration string, e.g. "6h0m0s". Empty means "use the controller default". */
  checkExistInterval?: string;
}

/** A single OCP/OKD release channel returned by GET /api/v1/releases/channels.
 *  Sourced from openshift/cincinnati-graph-data, with ConfigMap and built-in fallbacks. */
export interface OcpChannelEntry {
  name: string;    // e.g. "stable-4.18"
  type: string;    // "ocp" | "okd"
  version: string; // e.g. "4.18"
}
