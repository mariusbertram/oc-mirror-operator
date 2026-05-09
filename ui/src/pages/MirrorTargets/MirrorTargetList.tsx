import React, { useEffect, useMemo, useState } from 'react';
import {
  Alert,
  Button,
  EmptyState,
  EmptyStateBody,
  EmptyStateVariant,
  PageSection,
  SearchInput,
  Spinner,
  Text,
  TextContent,
  TextVariants,
  Toolbar,
  ToolbarContent,
  ToolbarItem,
} from '@patternfly/react-core';
import { Table, Thead, Tr, Th, Tbody, Td } from '@patternfly/react-table';
import { Link } from 'react-router-dom';
import { listTargets } from '../../api/client';
import type { TargetSummary } from '../../api/types';
import { StatusPill, computeStatus } from '../../components/StatusPill';
import { ProgressBar } from '../../components/ProgressBar';
import '../../components/plugin-styles.css';

export const MirrorTargetList: React.FC = () => {
  const [targets, setTargets] = useState<TargetSummary[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [search, setSearch] = useState('');

  const load = () => {
    setLoading(true);
    listTargets()
      .then(setTargets)
      .catch((e: Error) => setError(e.message))
      .finally(() => setLoading(false));
  };

  useEffect(() => {
    load();
    const interval = setInterval(load, 30_000);
    return () => clearInterval(interval);
  }, []);

  const filtered = useMemo(
    () =>
      targets.filter(
        (t) =>
          !search ||
          t.name.toLowerCase().includes(search.toLowerCase()) ||
          t.registry.toLowerCase().includes(search.toLowerCase()),
      ),
    [targets, search],
  );

  if (loading && targets.length === 0) {
    return (
      <PageSection>
        <Spinner />
      </PageSection>
    );
  }

  if (error) {
    return (
      <PageSection>
        <Alert variant="danger" title="Failed to load MirrorTargets" isInline>
          {error}
        </Alert>
      </PageSection>
    );
  }

  return (
    <PageSection>
      <TextContent style={{ marginBottom: 'var(--pf-v6-global--spacer--md)' }}>
        <Text component={TextVariants.h1}>MirrorTargets</Text>
        <Text component={TextVariants.p}>
          Each MirrorTarget defines a destination registry and the set of ImageSets to mirror into it.
        </Text>
      </TextContent>

      <Toolbar>
        <ToolbarContent>
          <ToolbarItem>
            <SearchInput
              placeholder="Filter by name or registry…"
              value={search}
              onChange={(_e, v) => setSearch(v)}
              onClear={() => setSearch('')}
            />
          </ToolbarItem>
          <ToolbarItem>
            <Button variant="secondary" onClick={load} isDisabled={loading}>
              {loading ? <Spinner size="sm" /> : 'Refresh'}
            </Button>
          </ToolbarItem>
          <ToolbarItem align={{ default: 'alignEnd' }} variant="pagination">
            <span className="mirror-toolbar-count">
              {filtered.length} of {targets.length}
            </span>
          </ToolbarItem>
        </ToolbarContent>
      </Toolbar>

      {filtered.length === 0 ? (
        <EmptyState variant={EmptyStateVariant.lg}>
          <TextContent>
            <Text component={TextVariants.h2}>
              {targets.length === 0 ? 'No MirrorTargets found' : 'No results match filter'}
            </Text>
          </TextContent>
          <EmptyStateBody>
            {targets.length === 0
              ? 'Create a MirrorTarget to declare a destination registry and start mirroring.'
              : 'Clear the filter to see all MirrorTargets.'}
          </EmptyStateBody>
        </EmptyState>
      ) : (
        <Table aria-label="MirrorTargets" variant="compact">
          <Thead>
            <Tr>
              <Th>Name</Th>
              <Th>Registry</Th>
              <Th>Status</Th>
              <Th style={{ minWidth: 220 }}>Progress</Th>
            </Tr>
          </Thead>
          <Tbody>
            {filtered.map((t) => {
              const status = computeStatus(t.totalImages, t.mirroredImages, t.pendingImages, t.failedImages);
              return (
                <Tr key={t.name}>
                  <Td dataLabel="Name">
                    <Link to={`/oc-mirror/targets/${t.name}`} className="mirror-link-strong">{t.name}</Link>
                    <div className="mirror-sub-text">{t.namespace}</div>
                  </Td>
                  <Td dataLabel="Registry">
                    <code className="mirror-mono">{t.registry}</code>
                  </Td>
                  <Td dataLabel="Status">
                    <StatusPill status={status} />
                  </Td>
                  <Td dataLabel="Progress">
                    <ProgressBar
                      total={t.totalImages}
                      mirrored={t.mirroredImages}
                      pending={t.pendingImages}
                      failed={t.failedImages}
                    />
                  </Td>
                </Tr>
              );
            })}
          </Tbody>
        </Table>
      )}
    </PageSection>
  );
};
