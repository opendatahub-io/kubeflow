import { TableColumn, TableConfig, ComponentValue } from 'kubeflow';
import { tableConfig } from '../config';
import { DeleteButtonComponent } from '../columns/delete-button/delete-button.component';
<<<<<<< HEAD

const customDeleteCol: TableColumn = {
  matHeaderCellDef: '',
=======
import { OpenPVCViewerButtonComponent } from '../columns/open-pvcviewer-button/open-pvcviewer-button.component';
import { ClosePVCViewerButtonComponent } from '../columns/close-pvcviewer-button/close-pvcviewer-button.component';

const customOpenPVCViewerCol: TableColumn = {
  matHeaderCellDef: '',
  matColumnDef: 'customOpenPVCViewer',
  style: { width: '40px' },
  value: new ComponentValue({
    component: OpenPVCViewerButtonComponent,
  }),
};

const customClosePVCViewerCol: TableColumn = {
  matHeaderCellDef: '',
  matColumnDef: 'customClosePVCViewer',
  style: { width: '40px' },
  value: new ComponentValue({
    component: ClosePVCViewerButtonComponent,
  }),
};

const customDeleteCol: TableColumn = {
  matHeaderCellDef: '',
>>>>>>> 48b8643bee14b8c85c3de9f6d129752bb55b44d3
  matColumnDef: 'customDelete',
  style: { width: '40px' },
  value: new ComponentValue({
    component: DeleteButtonComponent,
  }),
};

export const defaultConfig: TableConfig = {
  title: tableConfig.title,
  dynamicNamespaceColumn: true,
  newButtonText: tableConfig.newButtonText,
<<<<<<< HEAD
  columns: tableConfig.columns.concat(customDeleteCol),
=======
  columns: tableConfig.columns.concat(
    customOpenPVCViewerCol,
    customClosePVCViewerCol,
    customDeleteCol,
  ),
>>>>>>> 48b8643bee14b8c85c3de9f6d129752bb55b44d3
};
