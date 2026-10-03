import React from 'react';
import { Modal, ModalVariant } from '@patternfly/react-core';
import type { ConfirmationModalProps } from './types';

export const ConfirmationModal: React.FC<ConfirmationModalProps> = ({ title, isOpen, onClose, children, footer }) => (
  <Modal variant={ModalVariant.small} title={title} isOpen={isOpen} onClose={onClose} actions={[footer]}>
    {children}
  </Modal>
);
