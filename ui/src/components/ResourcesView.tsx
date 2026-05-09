import React, { useState } from 'react';
import {
  Button,
  Card,
  CardBody,
  CardHeader,
  CardTitle,
  EmptyState,
  EmptyStateBody,
  Flex,
  FlexItem,
  Label,
  Spinner,
  Text,
  TextContent,
  TextVariants,
} from '@patternfly/react-core';
import {
  CopyIcon,
  ExternalLinkAltIcon,
  EyeIcon,
  EyeSlashIcon,
} from '@patternfly/react-icons';
import { fetchRawText, getApiUrl } from '../api/client';
import type { ResourceLink } from '../api/types';
import './plugin-styles.css';

interface ResourceGroup {
  title: string;
  subtitle?: string;
  description: string;
  resources: ResourceLink[];
}

function groupResources(resources: ResourceLink[]): ResourceGroup[] {
  const mirrorConfig: ResourceLink[] = [];
  const catalogMap: Record<string, ResourceLink[]> = {};
  const signatures: ResourceLink[] = [];
  const other: ResourceLink[] = [];

  for (const r of resources) {
    const nameLower = r.name.toLowerCase();
    if (r.name === 'IDMS' || r.name === 'ITMS') {
      mirrorConfig.push(r);
    } else if (nameLower.startsWith('catalogsource') || nameLower.startsWith('clustercatalog')) {
      const m = r.name.match(/\(([^)]+)\)/);
      const slug = m ? m[1] : 'catalog';
      if (!catalogMap[slug]) catalogMap[slug] = [];
      catalogMap[slug].push(r);
    } else if (nameLower.startsWith('signatures')) {
      signatures.push(r);
    } else {
      other.push(r);
    }
  }

  const groups: ResourceGroup[] = [];

  if (mirrorConfig.length > 0) {
    groups.push({
      title: 'Mirror Configuration',
      description: 'Apply to the cluster to redirect image pulls to your mirror registry.',
      resources: mirrorConfig,
    });
  }

  for (const [slug, res] of Object.entries(catalogMap)) {
    groups.push({
      title: 'Catalog Sources',
      subtitle: slug,
      description: 'Apply to make the mirrored catalog available in OperatorHub.',
      resources: res,
    });
  }

  if (signatures.length > 0) {
    groups.push({
      title: 'Release Signatures',
      description: 'Apply to enable signature verification for mirrored release images.',
      resources: signatures,
    });
  }

  if (other.length > 0) {
    groups.push({
      title: 'Other Resources',
      description: 'Additional generated resources.',
      resources: other,
    });
  }

  return groups;
}

const TYPE_COLORS: Record<string, 'blue' | 'orange' | 'purple' | 'grey'> = {
  yaml: 'blue',
  json: 'orange',
};

interface ResourceRowProps {
  resource: ResourceLink;
}

const ResourceRow: React.FC<ResourceRowProps> = ({ resource }) => {
  const [expanded, setExpanded] = useState(false);
  const [content, setContent] = useState<string | null>(null);
  const [fetching, setFetching] = useState(false);
  const [copied, setCopied] = useState(false);
  const [fetchError, setFetchError] = useState<string | null>(null);

  const ensureContent = async (): Promise<string | null> => {
    if (content !== null) return content;
    setFetching(true);
    setFetchError(null);
    try {
      const text = await fetchRawText(resource.url);
      setContent(text);
      return text;
    } catch (e) {
      setFetchError((e as Error).message);
      return null;
    } finally {
      setFetching(false);
    }
  };

  const handleCopy = async () => {
    const text = await ensureContent();
    if (!text) return;
    try {
      await navigator.clipboard.writeText(text);
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    } catch {
      // Clipboard API unavailable — fall back to selection
    }
  };

  const handleTogglePreview = async () => {
    if (!expanded) await ensureContent();
    setExpanded((v) => !v);
  };

  const typeColor = TYPE_COLORS[resource.type] ?? 'grey';

  return (
    <div className="mirror-resource-row">
      <Flex
        alignItems={{ default: 'alignItemsCenter' }}
        flexWrap={{ default: 'nowrap' }}
        gap={{ default: 'gapSm' }}
      >
        <FlexItem>
          <Label isCompact color={typeColor}>{resource.type}</Label>
        </FlexItem>
        <FlexItem grow={{ default: 'grow' }}>
          <span className="mirror-resource-name">{resource.name}</span>
          <code className="mirror-mono mirror-resource-url">{resource.url}</code>
        </FlexItem>
        <FlexItem>
          <Flex gap={{ default: 'gapXs' }} flexWrap={{ default: 'nowrap' }}>
            <FlexItem>
              {fetching ? (
                <Spinner size="sm" />
              ) : (
                <Button
                  variant="plain"
                  size="sm"
                  title={copied ? 'Copied!' : 'Copy YAML to clipboard'}
                  onClick={handleCopy}
                  className={copied ? 'mirror-copy-btn--done' : undefined}
                >
                  <CopyIcon />
                  <span className="mirror-copy-label">{copied ? ' Copied' : ' Copy'}</span>
                </Button>
              )}
            </FlexItem>
            <FlexItem>
              <Button
                variant="plain"
                size="sm"
                title={expanded ? 'Hide preview' : 'Show preview'}
                onClick={handleTogglePreview}
              >
                {expanded ? <EyeSlashIcon /> : <EyeIcon />}
                <span className="mirror-copy-label">{expanded ? ' Hide' : ' View'}</span>
              </Button>
            </FlexItem>
            <FlexItem>
              <a href={getApiUrl(resource.url)} target="_blank" rel="noreferrer">
                <Button variant="plain" size="sm" title="Open raw in new tab">
                  <ExternalLinkAltIcon />
                </Button>
              </a>
            </FlexItem>
          </Flex>
        </FlexItem>
      </Flex>

      {fetchError && (
        <div className="mirror-resource-error">Failed to fetch: {fetchError}</div>
      )}

      {expanded && content !== null && (
        <pre className="mirror-yaml mirror-resource-preview">{content}</pre>
      )}
    </div>
  );
};

interface ResourceGroupCardProps {
  group: ResourceGroup;
}

const ResourceGroupCard: React.FC<ResourceGroupCardProps> = ({ group }) => (
  <Card isFlat>
    <CardHeader>
      <CardTitle>
        <Flex alignItems={{ default: 'alignItemsCenter' }} gap={{ default: 'gapSm' }}>
          <FlexItem>
            <TextContent>
              <Text component={TextVariants.h3}>{group.title}</Text>
            </TextContent>
          </FlexItem>
          {group.subtitle && (
            <FlexItem>
              <Label isCompact color="grey">{group.subtitle}</Label>
            </FlexItem>
          )}
        </Flex>
        <TextContent>
          <Text component={TextVariants.small}>{group.description}</Text>
        </TextContent>
      </CardTitle>
    </CardHeader>
    <CardBody style={{ paddingTop: 0 }}>
      <div className="mirror-resource-list">
        {group.resources.map((r) => (
          <ResourceRow key={r.url} resource={r} />
        ))}
      </div>
    </CardBody>
  </Card>
);

interface ResourcesViewProps {
  resources: ResourceLink[];
  emptyMessage?: string;
}

export const ResourcesView: React.FC<ResourcesViewProps> = ({
  resources,
  emptyMessage = 'No resources available yet. Resources are exposed once an ImageSet reaches Ready.',
}) => {
  if (resources.length === 0) {
    return (
      <EmptyState>
        <EmptyStateBody>{emptyMessage}</EmptyStateBody>
      </EmptyState>
    );
  }

  const groups = groupResources(resources);

  return (
    <Flex direction={{ default: 'column' }} gap={{ default: 'gapMd' }}>
      {groups.map((g, i) => (
        <FlexItem key={i}>
          <ResourceGroupCard group={g} />
        </FlexItem>
      ))}
    </Flex>
  );
};
