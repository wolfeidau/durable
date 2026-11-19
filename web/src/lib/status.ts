import { SubmissionStatus } from "../gen/durable/v1/durable_pb";

export const statusText: Record<number, string> = {
  [SubmissionStatus.QUEUED]: "queued",
  [SubmissionStatus.RUNNING]: "running",
  [SubmissionStatus.DONE]: "done",
  [SubmissionStatus.FAILED]: "failed",
  [SubmissionStatus.CANCELLED]: "cancelled",
};

export const statusTone: Record<number, string> = {
  [SubmissionStatus.QUEUED]: "tone-link",
  [SubmissionStatus.RUNNING]: "tone-link pulse",
  [SubmissionStatus.DONE]: "tone-ok",
  [SubmissionStatus.FAILED]: "tone-error",
  [SubmissionStatus.CANCELLED]: "tone-cancelled",
};

export const isActive = (s: SubmissionStatus): boolean => s === SubmissionStatus.QUEUED || s === SubmissionStatus.RUNNING;

export const isTerminal = (s: SubmissionStatus): boolean =>
  s === SubmissionStatus.DONE || s === SubmissionStatus.FAILED || s === SubmissionStatus.CANCELLED;
