import { ConnectError } from "@connectrpc/connect";

export const errText = (err: unknown): string => ConnectError.from(err).rawMessage;
