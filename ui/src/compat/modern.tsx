import React from 'react';
import { Modal, ModalBody, ModalFooter, ModalHeader, ModalVariant } from '@patternfly/react-core';
import type { ConfirmationModalProps } from './types';

export const ConfirmationModal: React.FC<ConfirmationModalProps> = ({ title, isOpen, onClose, children, footer }) => (
  <Modal variant={ModalVariant.small} aria-label={title} isOpen={isOpen} onClose={onClose}>
    <ModalHeader title={title} />
    <ModalBody>{children}</ModalBody>
    <ModalFooter>{footer}</ModalFooter>
  </Modal>
);
