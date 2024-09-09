from kubeflow.kubeflow.crud_backend import status
from werkzeug.exceptions import BadRequest
<<<<<<< HEAD
=======

>>>>>>> 48b8643bee14b8c85c3de9f6d129752bb55b44d3

def parse_tensorboard(tensorboard):
    """
    Process the Tensorboard object and format it as the UI expects it.
    """
    if tensorboard.get("status", {}).get("readyReplicas", 0) == 1:
        phase = status.STATUS_PHASE.READY
        message = "The Tensorboard server is ready to connect"
    else:
        phase = status.STATUS_PHASE.UNAVAILABLE
        message = "The Tensorboard server is currently unavailble"

    parsed_tensorboard = {
        "name": tensorboard["metadata"]["name"],
        "namespace": tensorboard["metadata"]["namespace"],
        "logspath": tensorboard["spec"]["logspath"],
        "age": tensorboard["metadata"]["creationTimestamp"],
        "status": status.create_status(phase, message, "")
    }

    return parsed_tensorboard


def get_tensorboard_dict(namespace, body):
    """
    Create Tensorboard object from request body and format it as a Python dict.
    """
    metadata = {
        "name": body["name"],
<<<<<<< HEAD
        "namespace": namespace, 
=======
        "namespace": namespace,
>>>>>>> 48b8643bee14b8c85c3de9f6d129752bb55b44d3
    }
    labels = get_tensorboard_configurations(body=body)
    if labels:
        metadata["labels"] = labels

    tensorboard = {
        "apiVersion": "tensorboard.kubeflow.org/v1alpha1",
        "kind": "Tensorboard",
        "metadata": metadata,
        "spec": {"logspath": body["logspath"]},
    }

    return tensorboard

<<<<<<< HEAD
def get_tensorboard_configurations(body):
    labels = body.get("configurations", None)
    cr_labels = {}
    
=======

def get_tensorboard_configurations(body):
    labels = body.get("configurations", None)
    cr_labels = {}

>>>>>>> 48b8643bee14b8c85c3de9f6d129752bb55b44d3
    if not isinstance(labels, list):
        raise BadRequest("Labels for PodDefaults are not list: %s" % labels)

    for label in labels:
        cr_labels[label] = "true"
<<<<<<< HEAD
    
    return cr_labels
=======

    return cr_labels
>>>>>>> 48b8643bee14b8c85c3de9f6d129752bb55b44d3
