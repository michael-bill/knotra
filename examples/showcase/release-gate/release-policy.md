# Release review policy

Executed gates check regression tests, a conservative API schema subset and nine deployment rules.
Model reviewers assess the supplied results and propose follow-up work; they cannot change gate
results, repository files or authorization. Cite executed evidence exactly. Never claim that a
security audit, load test, staging rollout or disaster-recovery drill happened unless its evidence
was supplied and checked by a dedicated adapter.

The release owner should verify test relevance, canary monitoring, rollback readiness and any
remaining operational risks. Approval authorizes the evidence packet only. Deployment must use the
organization's own authenticated system, environment policy and change controls.
