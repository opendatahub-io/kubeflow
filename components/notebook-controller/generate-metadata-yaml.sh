#!/usr/bin/env bash

set -uo pipefail

function trap_exit() {
	rc=$?

	set +x

	exit $rc
}

trap "trap_exit" EXIT

_derive_metadata()
{
  # inspired from https://stackoverflow.com/a/29835459
  current_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

  kf_project_file="${current_dir}/PROJECT"
  if [ -e "${kf_project_file}" ]; then

    if [ -z "${metadata_repo_url}" ]; then
      project_repo_reference=$(yq -e '.repo' "${kf_project_file}")
      project_repo_parts=( $(printf "%s" ${project_repo_reference##https://} | tr '/' ' ') )
      github_host="${project_repo_parts[0]}"
      github_owner="${project_repo_parts[1]}"
      github_repo="${project_repo_parts[2]}"

      metadata_repo_url=$(printf "https://%s/%s/%s" "${github_host}" "${github_owner}" "${github_repo}")
    fi

    if [ -z "${metadata_name}" ]; then
      project_domain=$(yq -e '.domain' "${kf_project_file}")
      project_name=$(yq -e '.projectName' "${kf_project_file}")
      metadata_name="${project_domain} ${project_name}"
    fi

  fi

  if [ -z "${metadata_version}" ]; then
    repo_root=$(git rev-parse --show-toplevel)
    version_file="${repo_root}/releasing/version/VERSION"

    metadata_version=$(cat "${version_file}" | head -n 1)
  fi
}

_fallback_to_existing_values()
{
  if [ -n "${existing_fallback}" ]; then
    if [ -z "${metadata_repo_url}" ]; then
      metadata_repo_url=$(yq -e '.releases[0].repoUrl' "${output_file}")
    fi

    if [ -z "${metadata_version}" ]; then
      metadata_version=$(yq -e '.releases[0].version' "${output_file}")
    fi

    if [ -z "${metadata_name}" ]; then
      metadata_name=$(yq -e '.releases[0].name' "${output_file}")
    fi
  fi
}

_check_for_missing_data()
{
  missing_data=

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

_handle_metadata_file()
{

  _derive_metadata

  _fallback_to_existing_values

  _check_for_missing_data

  # NOTE: Does not handle multiple entries!!
  yq_env_arg="${metadata_name}" yq -i '.releases[0].name = strenv(yq_env_arg)' "${output_file}"
  yq_env_arg="${metadata_version}" yq -i '.releases[0].version = strenv(yq_env_arg)' "${output_file}"
  yq_env_arg="${metadata_repo_url}" yq -i '.releases[0].repoUrl = strenv(yq_env_arg)' "${output_file}"
}

_usage()
{
	printf "%s\n" "Usage: $(basename $0) -o <output file> [-n <name>] [-v <version>] [-r <repoUrl>] [-p] [-x] [-h]"
}

_parse_opts()
{
	local OPTIND

	while getopts ':o:n:v:r:exh' OPTION; do
		case "${OPTION}" in
			o )
				output_file="${OPTARG}"

        if ! [ -e "${output_file}" ]; then
          touch "${output_file}"
        fi
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
			e )
				existing_fallback="t"
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

output_file=
metadata_repo_url=
metadata_version=
metadata_name=
existing_fallback=

if ! yq --version &> /dev/null; then
  printf "%s" "yq not installed... aborting script."
  exit 1
fi

_parse_opts "$@"

if [ -z "${output_file}" ]; then
  printf "%s" "-o <output file> argument is required"
  exit 1
fi

_handle_metadata_file