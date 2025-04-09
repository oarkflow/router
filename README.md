I need you to build a robust drag and drop website builder using dndkit, shadcn/ui and reactjs.
There should be 3 column layout. On leftsidebar, I would see list of components from which I could drag the component on canvas (dropzone). On leftsidebar, there should be a dropzone container as well with option to set columns after being dropped on root canvas.
In the middle there should be a dropzone canvas, allowing to drop any component from leftsidebar.
Once clicked on each dropped component, I should see the rightsidebar for it's configuration. Each component will have it's own set of configuration.

I should be able to move dropped component on canvas or container freely on canvas. I could drop components on Container or Canvas. The components could be dragged on to parent canvas or child container. I should also be able to resize the components as required occupying the place required.

Once the components are configured, I should be able to deploy the JSOn which could be rendered.

Currently the generated code, I could change the slider of columns but it's not affecting anything. After I configure columns on container, the components should only occupy the space within the column it's dropped on.

There should be drag and resize handler icon on each dropped component to make it flexible enough. Also the container should visually represent the number of columns it has with dashed border for each column around it.

There should be floating toolbar on each component for
Delete and Duplicate.

I should be able to move the child component on container to move around it's multiple columns. I should be able to drag the components between the columns. I should be able to move in any pixel of the bounded column.
Non components are showing resize handler/icon. I should be able to resize the components freely in different sizes. I should be resize components freely to any pixel.

1) Containers doesn't represent visually the number of columns.
2) When dragged and dropped from leftsidebar, there should have been animation.
3) IMPORTANT: I can't resize components freely. I should be able to resize.
4) The main canvas should shouldn't shrink down or up based on column changes. It should remain constant.

 Each component should represent it's own purpose. For e.g. Input, Button, Text, Image all have their own purpose should should be respected

I should be able to drop components on Container. I should also be moved within the container/canvas. on hovering on the container, the dragged/moved component should be linked as child of the container.
