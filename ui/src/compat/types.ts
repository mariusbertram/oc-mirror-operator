import type { ReactNode } from 'react';

export type ConfirmationModalProps = {
  title: string;
  isOpen: boolean;
  onClose: () => void;
  children: ReactNode;
  footer: ReactNode;
};
