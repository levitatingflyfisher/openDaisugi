/**
 * Port of contracts.py's `verify_delegation`, simplified: the conformance
 * wire protocol never carries `trusted_signers`/`signer_registry` (verify.py
 * calls `verify_delegation(envelope, contract, strict=strict, timeout_ms=…)`
 * with neither), and a SkillStep's `contract_envelope` carries no signature
 * field at all — so `signature_valid` is always `None` and every
 * signature-related branch in the oracle is dead for every case this client
 * will ever see. Only the subsumption decision matters here.
 */

import type { Envelope } from "./models.js";
import { envelopeSubsumes } from "./subsumption.js";

export interface DelegationDecision {
  allowed: boolean;
  unverifiedInvariants: string[];
}

export async function verifyDelegation(
  callerEnvelope: Envelope,
  skillEnvelope: Envelope,
  strict: boolean,
  timeoutMs: number,
  recognizedOpaqueTypes: ReadonlySet<string>
): Promise<DelegationDecision> {
  const sub = await envelopeSubsumes(callerEnvelope, skillEnvelope, timeoutMs, strict, recognizedOpaqueTypes);
  return { allowed: sub.holds, unverifiedInvariants: sub.unverifiedInvariants };
}
