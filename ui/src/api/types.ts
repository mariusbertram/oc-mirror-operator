export interface TargetSummary {
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
}

export interface CatalogSummary {
  slug: string;
  source: string;
  targetImage: string;
  filteredPackagesUrl: string;
  upstreamPackagesUrl: string;
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

export interface CatalogPackage {
  name: string;
  defaultChannel: string;
  channels: string[];
  description?: string;
}
