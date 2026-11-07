-- A creator can delete a finished video; its upload record stays for the audit trail, marked 'deleted'.
ALTER TYPE upload_status ADD VALUE IF NOT EXISTS 'deleted';
