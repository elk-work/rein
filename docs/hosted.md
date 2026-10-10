# Hosted Rein: first run by hand

This is a runbook for a person, not automation. None of these deployment steps
has been performed by the container build. Phase 0 proves `cloud-claude` in Elk
Scout against `elk-work/rein`; a customer launch and token broker come later.

Before starting, supply:

- The workspace's own Anthropic API key, with a spend limit; subscription login
  cannot fund this hosted run.
- An Elk Scout owner who can enrol `cloud-claude`, supply its workspace identifier
  and endpoint privately, obtain its scoped Elk token, queue one Do run, and
  accept the result through `review_run`.
- A GitHub administrator able to issue a token for **only** `elk-work/rein`.
- The registry/repository name and image version, a Cloud Run project
  (`elk-customer-data` for the proof), region, job name, and dedicated service
  account. The operator needs registry publishing and Cloud Run administration
  permissions; the runtime identity needs Secret Manager access only to the
  named secrets. Secret Manager and Cloud Run APIs must be enabled.
- Secret Manager secret names and numeric versions for the model key, Elk token,
  GitHub token, and deployment config; provide the values through the private
  console or approved credential tooling, never a command argument or report.
- A small Rein task that fits the GitHub token lifetime, billing access for
  compute and provider usage, and a reviewer who can finish the acceptance.

Never put credential values or private endpoint addresses in this public
repository. Do not enable shell tracing, echo environments, log token responses,
pass credentials as build arguments, or record them in deliverables.

1. In a clean checkout of the desired Rein commit, check the local toolchain:

   ```sh
   gcloud version
   docker version
   ```

   Set the non-secret deployment names in your private shell. `IMAGE` is a fully
   qualified registry image tag (default registry: GHCR,
   `ghcr.io/elk-work/rein-hosted`). `PROJECT`, `REGION`, `JOB`, `SERVICE_ACCOUNT`,
   `VERSION`, and each `*_SECRET` / `*_VERSION` variable below must be supplied.
   Use a unique job name for this proof. The image pins the Debian slim/Node 22
   and Go bases by digest and both npm tools by exact build-arg defaults;
   change pins through a PR. Apt packages use Debian's security repositories.

2. Build for Cloud Run's Linux amd64 platform, smoke-test, then publish manually
   using your approved registry login. CI never publishes an image, even on tags.

   ```sh
   docker build --platform linux/amd64 --build-arg "VERSION=$VERSION" -t "$IMAGE" .
   docker run --rm --network none --entrypoint rein "$IMAGE" version
   docker run --rm --network none --entrypoint claude "$IMAGE" --version
   docker run --rm --network none --entrypoint codex "$IMAGE" --version
   docker run --rm --network none --entrypoint id "$IMAGE" -u
   docker push "$IMAGE"
   ```

   The UID must be 10001. Record the published digest and use `IMAGE_DIGEST`
   (the full image reference ending in `@sha256:…`) for deployment. Make the GHCR
   package publicly pullable for this public proof image, or use an authorized
   Artifact Registry mirror if Cloud Run cannot import it. Do not bake registry
   credentials into the image.

3. In Elk Scout's Connected agents, enrol the Claude queue `cloud-claude` and
   obtain its scoped Elk credential privately. Preserve the enrolment's workspace
   and machine identifiers in the deployment config. Store that config as a
   Secret Manager file version mounted at `/etc/rein/config.toml`; the image has
   no default endpoint or credentials. The config has this shape (replace the
   placeholders privately, never commit the completed file):

   ```toml
   workspace = "WORKSPACE_IDENTIFIER"
   host_id = "cloud"
   machine_id = "ENROLLED_MACHINE_IDENTIFIER"
   work_dir = "/home/rein/work"

   [hosted]
   enabled = true
   max_run = "45m"
   allow_repos = ["elk-work/rein"]

   [elk]
   # Supply mcp_url privately from the enrolment.

   [[queues]]
   name = "cloud-claude"
   agent_kind = "claude"
   repo = "elk-work/rein"
   land = "pr"
   ```

   Store the workspace's model key and Elk token in separate Secret Manager
   versions. Grant the runtime service account Secret Accessor on these named
   secrets and the config only; no project-wide secret access.

4. Immediately before launch, have the GitHub administrator mint the proof
   token. Preferred: a write-capable GitHub App installed on `elk-work/rein`
   only; request an installation access token with `repositories` restricted
   explicitly to `["rein"]`, Contents write and Pull requests write (Metadata
   read is implicit). Use the administrator's approved private tooling to send
   the response directly to a new Secret Manager version; never print it.
   Elk Tap is read-only and cannot substitute for this App.

   If the App is unavailable, a person can create a fine-grained PAT in GitHub
   Settings with resource owner `elk-work`, **Only select repositories: rein**,
   Contents and Pull requests read/write, shortest available expiration and org
   approval. Transfer it privately to Secret Manager and revoke it after the
   proof. Grant no workflow write permission; choose a task that does not edit
   workflows. Installation tokens expire after one hour; this binary has no
   refresh broker, so the 45-minute Rein cap leaves time for startup and push.
   A two-hour Cloud Run timeout does not extend that token lifetime.

5. Create the job, referencing secret names and pinned numeric versions only.
   Cloud Run injects the three credentials into the process environment; the
   config is mounted as a file. No secret value appears in this command.

   ```sh
   gcloud run jobs create "$JOB" \
     --project "$PROJECT" --region "$REGION" \
     --image "$IMAGE_DIGEST" --service-account "$SERVICE_ACCOUNT" \
     --cpu 2 --memory 8Gi --tasks 1 --parallelism 1 \
     --task-timeout 2h --max-retries 1 \
     --set-secrets "ANTHROPIC_API_KEY=$MODEL_SECRET:$MODEL_VERSION,REIN_ELK_TOKEN=$ELK_SECRET:$ELK_VERSION,REIN_GITHUB_TOKEN=$GITHUB_SECRET:$GITHUB_VERSION,/etc/rein/config.toml=$CONFIG_SECRET:$CONFIG_VERSION"
   ```

   The maximum retry count permits a second task attempt after failure. Check
   Elk's run state before any manual re-execution; never queue a second proof
   just because a task failed. Container disk is memory-backed, including the
   repository and worktree; use a small task and watch peak memory.

6. In Elk Scout, dispatch exactly one Do run for that Rein task to `cloud-claude`.
   Then execute once, retaining the returned execution name privately:

   ```sh
   gcloud run jobs execute "$JOB" --project "$PROJECT" --region "$REGION" --wait
   ```

7. Verify the proof, without treating a zero exit status alone as acceptance:

   - The claimed run belongs to Elk Scout and the named Rein repository.
   - Its non-draft PR is open with green CI; the named reviewer accepts the
     deliverable through `review_run`.
   - Cloud Run execution details show completed tasks and no running instance.
     Retained job metadata is not an idle container. Delete the proof job after
     recording evidence if it is no longer needed.
   - Audit Cloud Logging, the run log, deliverable and PR for all three credential
     values. **Do not paste a key into a Cloud Logging query or grep argv.** In
     a private, access-controlled audit session, export only that execution's
     logs to a restricted temporary file. A local audit tool should read the
     secret versions through Secret Manager's API into memory, compare literal
     values against the export and report only pass/fail and match counts.
     Include encoded forms where relevant. Never print matching lines, secret
     values, or upload the audit inputs. Remove the export after auditing. A
     safe audit tool and an authorized auditor are required to complete this
     check; Rein's redaction alone is not proof that nothing leaked.
   - Record execution duration, attempts, region, CPU/memory allocation and actual
     compute charge next to the provider's token usage and charge for this run.
     If billing is delayed or cannot be attributed to the run, mark cost evidence
     pending. Do not substitute an estimate for the actual charge.
   - Revoke the proof GitHub token and remove obsolete secret versions using the
     organization's approved retention process. Record only public PR links,
     execution status, audit pass/fail and costs in the proof deliverable.

The image/runbook task is complete when CI builds and smoke-tests it. Phase 0
is complete only after a person performs the proof and all checks above pass.
No cloud resource, credential, registry publication or proof run is created by
this change.

References: [hosted plan](https://github.com/elk-work/elk/blob/main/docs/milestones/hosted-rein.md),
[Cloud Run secret configuration](https://docs.cloud.google.com/run/docs/configuring/jobs/secrets),
[job creation flags](https://docs.cloud.google.com/sdk/gcloud/reference/run/jobs/create),
[GitHub installation token scope](https://docs.github.com/en/enterprise-cloud@latest/apps/creating-github-apps/authenticating-with-github-apps/authenticating-as-a-github-app-installation).
