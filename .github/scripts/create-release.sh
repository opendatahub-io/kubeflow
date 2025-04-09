#! /usr/bin/env bash

## Description:
## 
## This script automates the process involved in creating a new release for the opendatahub-io/kubeflow repository as
## documented in https://issues.redhat.com/browse/RHOAIENG-15391.
##
## Usage: 
##      
##      create-release.sh
##          - Intended to be invoked as part of a GitHub Action workflow.
##          - Accepts no arguments.  Information is passed to the script in the form of environment variables (see below)
##  
##
## Required Environment Variables:
##
##      TARGET_BRANCH           [Optional] Existing branch on the repository used to construct the release.  If unset, the branch with most recent commit will be used.                                
##      RELEASE_TAG             [Optional] Tag to create that identifies the release.  If unset, latest existing tag will be incremented.
##      GITHUB_REPOSITORY       [Required] Repository to target for the release.  Set automatically as part of GitHub Actions execution.
##

set -euo pipefail

# Description: 
#   Computes the branch to use when creating the release.  If no argument provided for $1, the remote branch matching the naming convention of */v* that has the most
#   recent commit will be used.
#       git branch -r returns branches in the form of <remote>/<branch>
#            examples: 
#               origin/v1.10-branch  : match
#               origin/main          : no match    
#     
# Arguments: 
#   $1 : Release branch to use if provided.
#
# Returns:
#   Name of branch to use when creating the release
_get_release_branch()
{
    local release_branch="${1:-}"

    if [ -z "${release_branch}" ]; then
        local raw_branch=$(git branch -r --sort=-committerdate --list "*/v*" | head -n 1)
        local trimmed_branch="${raw_branch#${raw_branch%%[![:space:]]*}}"
        release_branch="${trimmed_branch#origin/}"
    fi

    printf "%s" "${release_branch}"
}

# Description: 
#   Retrieves the tag used for the most recent published release.  Draft and Pre-Release releases are excluded.
#
#   gh release list, by default, sorts releases based on the date of the most recent commit.  To ensure the most recent published release is returned, a jq filter
#   is used on the results to enforce ordering by publish time.  Under most circumstances, this ordering should be identical.
#
# Returns:
#   Name of tag used for the most recent published release
_get_latest_release_tag()
{
    gh release list \
        --repo "$GITHUB_REPOSITORY" \
        --exclude-drafts \
        --exclude-pre-releases \
        --json tagName,publishedAt \
        --jq 'sort_by(.publishedAt) | reverse | .[0].tagName'
}

# Description: 
#   Determines if the most recent release tag is related to the given release branch.  A release branch is "related" to the most recent release tag if the tag name starts 
#   with the branch prefix.
#
#   This determination is critical in being able to properly auto-increment the release name. See comments on _get_target_release_json() for further details.
#
# Arguments: 
#   $1 : Branch prefix that is being used to create the new release.  kubeflow names branches like 'v1.10-branch'.  The branch prefix would then be expected to be 'v1.10'
#   $2 : Tag name corresponding to the most recent published release
#
# Returns:
#   0 if the release branch name is aligned with the latest release tag
#   1 otherwise
_same_branch_as_prior_release()
{
    local release_branch_prefix="${1:-}"
    local latest_release_tag="${2:-}"

    case "${latest_release_tag}" in 
        "${release_branch_prefix}"*) 
            true
            ;; 
        *) 
            false
            ;; 
    esac
}

# Description: 
#   Determines the name of the release (which is also the tag name) to identify the to-be-created release.  Additionally returns the tag name of the most recent
#   published release to use in automated notes generation.
#       As both these data points can be interdependent and require querying for the latest published release, the computation is combined in this single function
#
#   Release names are expected to be in the form: v{major}.{minor}.{patch}-{release}
#
#   If a release name is provided in the arguments, it is used as-is without any further computation.  However, if a release name is not provided, this function will
#   analyze the state of the release branch as well as the most recent published release tag, to determine the appropriate auto-incrementing strategy. Consider the 
#   following scenarios:
#       _get_target_release_json "v1.10-branch" ""
#           In this case, the most recent published release is retrieved, and, based on _same_branch_as_prior_release():
#               - if the most recent published release is associated with the release branch, the {release} segment of the name is incremented by 1 (ex: v1.10.0-5 to v1.10.0-6)
#               - if the most recent published release is not associated with the release branch, release name is "reset" based on the branch (ex: v1.10.0-1)
#
#       _get_target_release_json "v1.10-branch" "v1.10.0-5"
#            In this case, the release name is simply the provided 'v1.10.0-5' argument.  If this release already exists, the script will subsequently error.
#
#   Generally speaking, it should not be necessary to provide the $2 argument unless under extraordinary circumstances.
#
# Arguments: 
#   $1 : Name of the branch being used to create the new release.
#   $2 : Name of the release (and related tag)
#
# Returns:
#   Stringified JSON of the form: '{notes_start_tag: <previous release tag>, release_name: <release name>}'
_get_target_release_json()
{
    local release_branch="${1:-}"
    local release_name="${2:-}"

    local latest_release_tag=$(_get_latest_release_tag) 
    local notes_start_tag="${latest_release_tag}"
    if [ -z "${release_name}" ]; then
        local release_base="${release_branch%-branch}"
        if ! _same_branch_as_prior_release "${release_base}" "${latest_release_tag}"; then
            latest_release_tag="${release_base}.0-0"

            if [ -z "${latest_release_tag}" ]; then
                notes_start_tag=
            fi
        fi

        tag_parts=($(printf "%s" "${latest_release_tag}" | tr '-' ' '))
        release_prefix="${tag_parts[0]}"
        release_id="$(( ${tag_parts[1]} + 1 ))"
        release_name="${release_prefix}-${release_id}"
    fi

    jq -n --arg notes_start_tag "${notes_start_tag}" --arg release_name "${release_name}" '{notes_start_tag: $notes_start_tag, release_name: $release_name}'
}

# Description: 
#   Invokes the GH CLI to create a release.  Release notes are automatically generated.  If the $3 argument is not provided, the --notes-start-tag option is not
#   provided to the GH CLI invocation.
#
#   Expects GITHUB_REPOSITORY to be defined as an environment variable in the shell session.
#
# Arguments: 
#   $1 : Name of the branch being used to create the new release.
#   $2 : Name of the release (and related tag)
#   $3 : Name of the tag to use, if provided, for the --notes-start-tag parameter of the 'gh release create' command
_create_release()
{
    local release_branch="${1:-}"
    local release_name="${2:-}"
    local notes_start_tag="${3:-}"

    gh release create "${release_name}" \
        --repo "$GITHUB_REPOSITORY" \
        --title "${release_name}" \
        --target "${release_branch}" \
        --generate-notes \
        ${notes_start_tag:+ --notes-start-tag ${notes_start_tag}}
}

# Description: 
#   Orchestration logic that accomplishes the intent of the script. Diagnostic messages are also output to aid in understanding the outcome.
#
#   Will honor TARGET_BRANCH and RELEASE_TAG environment variables if defined in the shell session.
main()
{
    release_branch=$( _get_release_branch "${TARGET_BRANCH}" )

    echo "Using branch '${release_branch}'"

    target_release_json=$( _get_target_release_json "${release_branch}" "${RELEASE_TAG}" )
    release_name=$( jq -r '.release_name' <<< "${target_release_json}" )
    notes_start_tag=$( jq -r '.notes_start_tag' <<< "${target_release_json}" )

    echo "Using release name '${release_name}' ${notes_start_tag:+with a start tag of '${notes_start_tag}' for notes generation}"

    _create_release "${release_branch}" "${release_name}" "${notes_start_tag}"
}


main


