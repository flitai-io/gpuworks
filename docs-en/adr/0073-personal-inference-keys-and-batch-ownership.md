# ADR 0073: Inference keys belong to users; legacy zero-user batches stay scoped to their key

Date: 2026-11-18 · Status: accepted

## Background

Console batches record the current JWT user, while management-created inference keys have used user_id=0. A user's key therefore cannot read their console batch, and legacy zero-user keys collapse into one resource scope. The database already retains user_id and api_key_id, but cannot reliably identify the creator of a legacy key.

## Decision

1. **New key ownership comes from authenticated management identity.** HTTP creation uses the current JWT user, not a user_id supplied in the body. Normal users manage their own keys and administrators retain site-wide management. Listing, scope edits and revocation enforce the same ownership rule on the server.
2. **Positive-user batch resources remain user-owned.** As documented, any valid inference key of a user may read that user's batches and files; other users receive not-found responses. Deployment scope continues to constrain inference execution; this change does not introduce a separate historical-data read ACL.
3. **Zero user does not mean all users.** Legacy user_id=0 batches and files must also match their original api_key_id. SQL listing and individual-resource checks use the same rule. Anonymous key0/user0 callers cannot read another legacy key's resources. Site-wide console access remains an explicit administrator permission.
4. **Legacy keys are not automatically reassigned.** Do not assign them to the first administrator, current caller or last user, or infer ownership from names. Keep their inference validity and deployment scope. Reissue through existing operations: create a personal key, replace client credentials, and explicitly revoke the old key. No claim endpoint or ownership table is added. A personal replacement does not automatically inherit legacy batches of unproven ownership; administrators can still manage historical tasks through the console.
5. **Ownership display uses existing facts.** Key metadata includes user_id with zero retained. The UI may explain legacy keys and replacement, but this display field grants no authority.

## Trade-offs

- Replacing only the creation-time zero does not fix cross-key legacy access or cross-user key management. Creation, reads and management must agree on identity.
- Automatic migration lacks a trusted creator and could transfer someone else's data. Explicit reissue preserves legacy inference compatibility and administrator management without guessing.
- Reuse UserID/APIKeyID rather than adding migrations, a generic ACL or a claim workflow. Combining deployment-call scope with historical read scope is a separate product decision; this change preserves the existing user-resource contract.

## Verification

Create keys through real HTTP and read console batches and input/output files. Same-user keys, including restricted keys, retain documented read access while execution scope remains enforced. Other users, anonymous callers and other legacy keys cannot list, read, cancel or delete unrelated resources. Verify normal-user management isolation, administrator management, authentication after revocation and legacy inference validity. Fixtures must not bypass HTTP creation by supplying an ideal nonzero user directly.

<!-- translation-source-sha256: 73dc5728b56c2927a1f4ac55befbf6ce4023136bc6d0c26150ddd538896a3b0f # ADR 0073: Inference keys belong to users; legacy zero-user batches stay scoped to their key -->
<!-- translation-source-sha256: 0af51490dc86025ff65bd6ed82ec4bf80b4414736e0feb2ce1140a8d6d2f9a5a ## Background -->
<!-- translation-source-sha256: fb7c79d88a644f96e8afbe5da58ffd7e38ea4a75a36b263591f33fc8d65afac0 ## Decision -->
<!-- translation-source-sha256: cd73da45476fc89bc00ba22ceccbe4ec22593b29c314dd29cda6b0a0f87e2bae ## Trade-offs -->
<!-- translation-source-sha256: 9c038c518a356f55495a9796a3c59421c03ca05bf63723f7a8138edd01129b3d ## Verification -->
