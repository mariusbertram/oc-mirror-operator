import React, { useEffect, useState } from 'react';
import {
  Alert,
  Button,
  EmptyState,
  EmptyStateBody,
  EmptyStateVariant,
  PageSection,
  Progress,
  Spinner,
  Title,
} from '@patternfly/react-core';
import {
  Table,
  Thead,
  Tr,
  Th,
  Tbody,
  Td,
} from '@patternfly/react-table';
import { Link } from 'react-router-dom';
import { listTargets } from '../../api/client';
import type { TargetSummary } from '../../api/types';

export const MirrorTargetList: React.FC = () => {
  const [targets, setTargets] = useState<TargetSummary[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

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

  if (targets.length === 0) {
    return (
      <PageSection>
        <EmptyState variant={EmptyStateVariant.full}>
          <Title headingLevel="h2">No MirrorTargets found</Title>
          <EmptyStateBody>No MirrorTargets exist in this namespace.</EmptyStateBody>
        </EmptyState>
      </PageSection>
    );
  }

  return (
    <PageSection>
      <Title headingLevel="h1" style={{ marginBottom: '1rem' }}>
        Mirror Targets
        <Button variant="plain" onClick={load} style={{ marginLeft: '0.5rem' }} isDisabled={loading}>
          {loading ? <Spinner size="sm" /> : '↻'}
        </Button>
      </Title>
      <Table aria-label="MirrorTargets">
        <Thead>
          <Tr>
            <Th>Name</Th>
            <Th>Registry</Th>
            <Th>Progress</Th>
            <Th>Total</Th>
            <Th>Mirrored</Th>
            <Th>Pending</Th>
            <Th>Failed</Th>
          </Tr>
        </Thead>
        <Tbody>
          {targets.map((t) => {
            const pct = t.totalImages > 0 ? (t.mirroredImages / t.totalImages) * 100 : 0;
            return (
              <Tr key={t.name}>
                <Td>
                  <Link to={`/targets/${t.name}`}>{t.name}</Link>
                </Td>
                <Td>{t.registry}</Td>
                <Td style={{ minWidth: '160px' }}>
                  <Progress
                    value={pct}
                    title={`${Math.round(pct)}%`}
                    measureLocation="outside"
                    aria-label={`${t.name} progress`}
                  />
                </Td>
                <Td>{t.totalImages}</Td>
                <Td>{t.mirroredImages}</Td>
                <Td>{t.pendingImages}</Td>
                <Td style={{ color: t.failedImages > 0 ? 'var(--pf-v5-global--danger-color--100)' : undefined }}>
                  {t.failedImages}
                </Td>
              </Tr>
            );
          })}
        </Tbody>
      </Table>
    </PageSection>
  );
};
