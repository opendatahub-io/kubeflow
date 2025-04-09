#! /usr/bin/env bash

## Description:
## 
## This script will ensure a components/notebook-controller/config/component_metadata.yaml file exists and is compliant with
## https://issues.redhat.com/browse/RHOAISTRAT-327.
##    - Lots of discussion around the acceptance criteria from this work was also captured in Slack
##      - see: https://redhat-internal.slack.com/archives/C05NXTEHLGY/p1731327116001979
##
## By default, information to populate component_metadata.yaml is extracted from 2 files in the repo:
##    - ./components/notebook-controller/PROJECT
##    - ./releasing/version/VERSION
##
## If component_metadata.yaml file exists, and attempting to generate any of the required attributes results in an empty string, 
## then the existing value is preserved.  
##
## The script is designed to generate appropriate attribute values implicitly.  It should be able to be executed without providing
## any of the optional arguments.  In the event an optional argument is provided on the command line, that value is used as-is.
##
## It should be noted that while the component_metadata.yaml specification has a root level attribute of 'releases' that is a list,
## this script only interacts with index 0.
##
## Dependencies: 
##     
##    - yq:   https://mikefarah.gitbook.io/yq
##
## Usage: 
##
##      generate-metadata-yaml.sh [-o <output file>] [-n <name>] [-v <version>] [-r <repoUrl>] [-p] [-x] [-h]"
##          - Intended (eventually) to be invoked as part of a GitHub Action workflow.
##          - Arguments
##              - [optional] -o <output file>
##                  - where the script will write its output
##                  - defaults to ./components/notebook-controller/config/component-metadata.yaml
##              - [optional] -n <name>
##                  - name attribute of 'releases' element at index 0
##                  - defaults to a value derived from the 'domain' and 'projectName' attribute of ./components/notebook-controller/PROJECT
##                      - value of 'domain' is split on the '.' character, and only first word is uppercased, discarding all other words
##                        - ex: kubeflow.org -> Kubeflow
##                      - value of 'projectName' is split on the '-' character, each word has the 1st letter uppercased, and then 
##                      results are joined back together delimited by whitespace
##                        - ex: notebook-controller -> Notebook Controller
##              - [optional] -v <version>
##                  - version attribute of 'releases' element at index 0
##                  - defaults to a value derived from contents of ./releasing/version/VERSION
##                      - 1st line of VERSION is read, and any leading 'v' character is removed from content
##                        - ex: v1.10.0 -> 1.10.0
##              - [optional] -r <repoUrl>
##                  - repoUrl attribute of 'releases' element at index 0
##                  - defaults to a value derived from the 'repo' attribute of ./components/notebook-controller/PROJECT
##                      - value of 'repo' is split on the '/' character, 1st 3 elements are joined together by the '/' character, 
##                      and then 'https://' prefix is added
##                        - ex: github.com/kubeflow/kubeflow/components/notebook-controller -> https://github.com/kubeflow/kubeflow
##              - [optional] -x 
##                  - enables tracing on the shell script
##              - [optional] -h
##                  - prints a simple usage message
##  
##


set -uo pipefail

# Description: 
#   Simple trap function that ensures shell tracing is disabled upon script exit.
function trap_exit() {
	rc=$?

	set +x

	exit $rc
}

trap "trap_exit" EXIT

# Description: 
#   Helper function that gets invoked when '-h' passed as a command line argument.
#
# Returns:
#   Simple string outlining functionality of the script
_usage()
{
	printf "%s\n" "Usage: $(basename "${0}") -o <output file> [-n <name>] [-v <version>] [-r <repoUrl>] [-p] [-x] [-h]"
}

# Description: 
#   Computes the default component_metadata.yaml 'name', 'version', and 'repoUrl' attributes for the 0th element of 'releases'.  If the
#   value of any attribute was provided on the command line, that value is used as-is and subsequent processing is skipped.
#
# Outputs:
#   metadata_repo_url
#   metadata_name
#   metadata_version
_derive_metadata()
{

  local kf_project_file="${current_dir}/PROJECT"
  if [ -e "${kf_project_file}" ]; then

    if [ -z "${metadata_repo_url}" ]; then
      local project_repo_reference=
      project_repo_reference=$(yq -e '.repo' "${kf_project_file}")
      readarray -td/ project_repo_parts <<< "${project_repo_reference##https://}/"
      local github_host="${project_repo_parts[0]}"
      local github_owner="${project_repo_parts[1]}"
      local github_repo="${project_repo_parts[2]}"

      metadata_repo_url=$(printf "https://%s/%s/%s" "${github_host}" "${github_owner}" "${github_repo}")
    fi

    if [ -z "${metadata_name}" ]; then

      readarray -td. org_parts <<< "$(yq e '.domain' "${kf_project_file}")-"
      unset 'org_parts[-1]'
      org_parts[0]="$(tr '[:lower:]' '[:upper:]' <<< "${org_parts[0]:0:1}")${org_parts[0]:1}"

      readarray -td- name_parts <<< "$(yq e '.projectName' "${kf_project_file}")-"
      unset 'name_parts[-1]'
      for i in "${!name_parts[@]}"; do
        name_parts[i]="$(tr '[:lower:]' '[:upper:]' <<< "${name_parts[i]:0:1}")${name_parts[i]:1}"
      done
      metadata_name="$(printf '%s' "${org_parts[0]} ${name_parts[*]}")"
    fi

  fi

  if [ -z "${metadata_version}" ]; then
    local repo_root=
    repo_root=$(git rev-parse --show-toplevel)
    local version_file="${repo_root}/releasing/version/VERSION"

    local raw_version=
    raw_version=$(head -n 1 < "${version_file}")
    metadata_version="${raw_version##v}"
  fi
}

# Description: 
#   Computes the component_metadata.yaml 'name', 'version', and 'repoUrl' attributes for the 0th element of 'releases'
#   based on an existing component_metadata.yaml file.  Processing is skipped for any output variable that already has
#   a non-zero length string value.
#
# Outputs:
#   metadata_repo_url
#   metadata_name
#   metadata_version
_fallback_to_existing_values()
{
  if [ -e "${output_file}" ]; then
    if [ -z "${metadata_repo_url}" ]; then
      metadata_repo_url=$(yq -e '.releases[0].repoUrl // ""' "${output_file}")
    fi

    if [ -z "${metadata_version}" ]; then
      metadata_version=$(yq -e '.releases[0].version // ""' "${output_file}")
    fi

    if [ -z "${metadata_name}" ]; then
      metadata_name=$(yq -e '.releases[0].name // ""' "${output_file}")
    fi
  fi
}

# Description: 
#   Validation function that ensures all required attributes have non-zero length.  Any attributes in violation of
#   this check will log an error message and cause script to exit with a 1 status code.
#
_check_for_missing_data()
{
  local missing_data=

  if [ -z "${metadata_repo_url}" ]; then
    printf "%s\n" "repoUrl attribute not specified and unable to be inferred"
    missing_data=1
  fi

  if [ -z "${metadata_version}" ]; then
    printf "%s\n" "version attribute not specified and unable to be inferred"
    missing_data=1
  fi

  if [ -z "${metadata_name}" ]; then
    printf "%s\n" "name attribute not specified and unable to be inferred"
    missing_data=1
  fi

  if [ -n "${missing_data}" ]; then
    exit 1
  fi
}

# Description: 
#   Orchestration logic that generates the component_metadata.yaml file.
#
#   NOTE: Multiple entries for the 'releases' attribute is not supported. Only the 0th index is operated against.
#
_handle_metadata_file()
{

  _derive_metadata

  _fallback_to_existing_values

  _check_for_missing_data

  yq_env_arg="${metadata_name}" yq -i '.releases[0].name = strenv(yq_env_arg)' "${output_file}"
  yq_env_arg="${metadata_version}" yq -i '.releases[0].version = strenv(yq_env_arg)' "${output_file}"
  yq_env_arg="${metadata_repo_url}" yq -i '.releases[0].repoUrl = strenv(yq_env_arg)' "${output_file}"
}

# Description: 
#   Helper function that processes command line arguments provided to the script.
#       - '-h' will cause script to exit with 0 (successful) status code
#       - any unsupported options will cause script to exit with 1 (failure) status code
#
# Outputs:
#   output_file
#   metadata_repo_url
#   metadata_name
#   metadata_version
_parse_opts()
{
	local OPTIND

	while getopts ':o:n:v:r:exh' OPTION; do
		case "${OPTION}" in
			o )
				output_file="${OPTARG}"
        ;;
			n )
				metadata_name="${OPTARG}"
				;;
			v )
				metadata_version="${OPTARG}"
				;;
			r )
				metadata_repo_url="${OPTARG}"
				;;
			h)
				_usage
				exit
				;;
			* )
				_usage
				exit 1
				;;
		esac
	done
}

# inspired from https://stackoverflow.com/a/29835459
current_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
output_file="${current_dir}/config/component_metadata.yaml"
metadata_repo_url=
metadata_version=
metadata_name=

if ! yq --version &> /dev/null; then
  printf "%s" "yq not installed... aborting script."
  exit 1
fi

_parse_opts "$@"


if ! [ -e "${output_file}" ]; then
  touch "${output_file}"
fi

_handle_metadata_file