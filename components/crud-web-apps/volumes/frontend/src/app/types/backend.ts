import { Params } from '@angular/router';
import { V1PersistentVolumeClaim, V1Pod } from '@kubernetes/client-node';
import { Status, BackendResponse, STATUS_TYPE } from 'kubeflow';
import { EventObject } from './event';

export interface VWABackendResponse extends BackendResponse {
  pvcs?: PVCResponseObject[];
  pvc?: V1PersistentVolumeClaim;
  events?: EventObject[];
  pods?: V1Pod[];
}

export interface PVCResponseObject {
  age: {
    uptime: string;
    timestamp: string;
  };
  capacity: string;
  class: string;
  modes: string[];
  name: string;
  namespace: string;
  status: Status;
  notebooks: string[];
<<<<<<< HEAD
=======
  viewer: {
    status: STATUS_TYPE;
    url: string;
  };
>>>>>>> 48b8643bee14b8c85c3de9f6d129752bb55b44d3
}

export interface PVCProcessedObject extends PVCResponseObject {
  deleteAction?: string;
  editAction?: string;
  closePVCViewerAction?: string;
  openPVCViewerAction?: string;
  ageValue?: string;
  ageTooltip?: string;
  link: {
    text: string;
    url: string;
    queryParams?: Params | null;
  };
}

export interface PVCPostObject {
  name: string;
  type: string;
  size: string | number;
  class: string;
  mode: string;
  snapshot: string;
}
