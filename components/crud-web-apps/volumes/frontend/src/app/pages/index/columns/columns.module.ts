import { NgModule } from '@angular/core';
import { CommonModule } from '@angular/common';
import { MatTooltipModule } from '@angular/material/tooltip';
import { DeleteButtonComponent } from './delete-button/delete-button.component';
<<<<<<< HEAD
=======
import { OpenPVCViewerButtonComponent } from './open-pvcviewer-button/open-pvcviewer-button.component';
import { ClosePVCViewerButtonComponent } from './close-pvcviewer-button/close-pvcviewer-button.component';
>>>>>>> 48b8643bee14b8c85c3de9f6d129752bb55b44d3
import { IconModule, KubeflowModule, UrlsModule } from 'kubeflow';
import { UsedByComponent } from './used-by/used-by.component';

@NgModule({
<<<<<<< HEAD
  declarations: [DeleteButtonComponent, UsedByComponent],
=======
  declarations: [
    OpenPVCViewerButtonComponent,
    ClosePVCViewerButtonComponent,
    DeleteButtonComponent,
    UsedByComponent,
  ],
>>>>>>> 48b8643bee14b8c85c3de9f6d129752bb55b44d3
  imports: [
    CommonModule,
    MatTooltipModule,
    IconModule,
    KubeflowModule,
    UrlsModule,
  ],
<<<<<<< HEAD
  exports: [DeleteButtonComponent, UsedByComponent],
=======
  exports: [
    OpenPVCViewerButtonComponent,
    ClosePVCViewerButtonComponent,
    DeleteButtonComponent,
    UsedByComponent,
  ],
>>>>>>> 48b8643bee14b8c85c3de9f6d129752bb55b44d3
})
export class ColumnsModule {}
