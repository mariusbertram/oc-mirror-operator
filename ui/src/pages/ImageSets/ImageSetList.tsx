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
  Title,
  Toolbar,
  ToolbarContent,
  ToolbarItem,
} from '@patternfly/react-core';
import { Table, Thead, Tr, Th, Tbody, Td } from '@patternfly/react-table';
import { Link } from 'react-router-dom';
import { listTargets, getTarget } from '../../api/client';
import type { ImageSetSummary, TargetSummary } from '../../api/types';
import { StatusPill, computeStatus } from '../../components/StatusPill';
import { ProgressBar } from '../../components/ProgressBar';
import '../../components/plugin-styles.css';

interface ImageSetRow extends ImageSetSummary {
  targetName: string;
  targetNamespace: string;
}

export const ImageSetList: React.FC = () => {
  const [rows, setRows] = useState<ImageSetRow[]>([]);
  const [targets, setTargets] = useState<TargetSummary[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [search, setSearch] = useState('');
  const [filterTarget, setFilterTarget] = useState('All');

  const load = async () => {
    setLoading(true);
    setError(null);
    try {
      const ts = await listTargets();
      setTargets(ts);
      const details = await Promise.all(ts.map((t) => getTarget(t.name)));
      const allRows: ImageSetRow[] = details.flatMap((d) =>
        d.imageSets.map((is) => ({
          ...is,
          targetName: d.name,
          targetNamespace: d.namespace,
        })),
      );
      setRows(allRows);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    load();
    const interval = setInterval(load, 30_000);
    return () => clearInterval(interval);
  }, []);

  const filtered = useMemo(
    () =>
      rows.filter(
        (is) =>
          (filterTarget === 'All' || is.targetName === filterTarget) &&
          (!search || is.name.toLowerCase().includes(search.toLowerCase())),
      ),
    [rows, search, filterTarget],
  );

  if (loading && rows.length === 0) {
    return <PageSection><Spinner /></PageSection>;
  }

  if (error) {
    return (
      <PageSection>
        <Alert variant="danger" title="Failed to load ImageSets" isInline>{error}</Alert>
      </PageSection>
    );
  }

  return (
    <PageSection>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '1rem' }}>
        <div>
          <Title headingLevel="h1">ImageSets</Title>
          <p style={{ margin: '4px 0 0', color: 'var(--pf-v5-global--Color--200)' }}>
            An ImageSet declares a slice of releases, operator catalogs, and additional images to mirror.
          </p>
        </div>
        <Button variant="secondary" onClick={load} isDisabled={loading}>Refresh</Button>
      </div>

      <Toolbar style={{ marginBottom: 0, paddingLeft: 0 }}>
        <ToolbarContent>
          <ToolbarItem>
            <SearchInput
              placeholder="Filter by name…"
              value={search}
              onChange={(_e, v) => setSearch(v)}
              onClear={() => setSearch('')}
            />
          </ToolbarItem>
          <ToolbarItem>
            <select
              value={filterTarget}
              onChange={(e) => setFilterTarget(e.target.value)}
              style={{
                padding: '6px 10px',
                border: '1px solid var(--pf-v5-global--BorderColor--100)',
                borderRadius: 3,
                background: 'var(--pf-v5-global--BackgroundColor--100)',
                color: 'inherit',
                font: 'inherit',
                fontSize: 14,
                minWidth: 220,
              }}
            >
              <option value="All">All targets</option>
              {targets.map((t) => (
                <option key={t.name} value={t.name}>{t.name}</option>
              ))}
            </select>
          </ToolbarItem>
          <ToolbarItem align={{ default: 'alignRight' }}>
            <span style={{ fontSize: 13, color: 'var(--pf-v5-global--Color--200)' }}>
              {filtered.length} of {rows.length}
            </span>
          </ToolbarItem>
        </ToolbarContent>
      </Toolbar>

      {filtered.length === 0 ? (
        <EmptyState variant={EmptyStateVariant.full}>
          <Title headingLevel="h2">{rows.length === 0 ? 'No ImageSets found' : 'No results match filter'}</Title>
          <EmptyStateBody>
            {rows.length === 0
              ? 'Create an ImageSet and assign it to a MirrorTarget to start mirroring.'
              : 'Clear the filter to see all ImageSets.'}
          </EmptyStateBody>
        </EmptyState>
      ) : (
        <Table aria-label="ImageSets" variant="compact">
          <Thead>
            <Tr>
              <Th>Name</Th>
              <Th>MirrorTarget</Th>
              <Th>Status</Th>
              <Th style={{ minWidth: 200 }}>Progress</Th>
              <Th>Total</Th>
              <Th>Mirrored</Th>
              <Th>Failed</Th>
              <Th>Resources</Th>
            </Tr>
          </Thead>
          <Tbody>
            {filtered.map((is) => {
              const status = computeStatus(is.total, is.mirrored, is.pending, is.failed);
              return (
                <Tr key={`${is.targetName}/${is.name}`}>
                  <Td>
                    <Link to={`/oc-mirror/targets/${is.targetName}/imagesets/${is.name}`} style={{ fontWeight: 500 }}>
                      {is.name}
                    </Link>
                  </Td>
                  <Td>
                    <Link to={`/oc-mirror/targets/${is.targetName}`}>
                      <span className="mirror-tag">{is.targetName}</span>
                    </Link>
                  </Td>
                  <Td>
                    <StatusPill status={status} />
                  </Td>
                  <Td>
                    <ProgressBar total={is.total} mirrored={is.mirrored} pending={is.pending} failed={is.failed} />
                  </Td>
                  <Td style={{ fontVariantNumeric: 'tabular-nums' }}>{is.total.toLocaleString()}</Td>
                  <Td style={{ fontVariantNumeric: 'tabular-nums', color: 'var(--pf-v5-global--success-color--100)' }}>
                    {is.mirrored.toLocaleString()}
                  </Td>
                  <Td style={{ fontVariantNumeric: 'tabular-nums', color: is.failed > 0 ? 'var(--pf-v5-global--danger-color--100)' : undefined }}>
                    {is.failed.toLocaleString()}
                  </Td>
                  <Td style={{ fontVariantNumeric: 'tabular-nums', color: 'var(--pf-v5-global--Color--200)' }}>
                    {is.resources.length}
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
